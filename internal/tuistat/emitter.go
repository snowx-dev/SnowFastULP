package tuistat

import (
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// minEmitInterval keeps a mistyped -json-every from hot-looping the process.
const minEmitInterval = 200 * time.Millisecond

// phasePollInterval bounds how late a phase transition (checking-history →
// extracting/ingesting → done) can appear in the stream: the loop polls the
// provider this often and emits immediately when the phase changed, while
// plain within-phase update ticks keep the -json-every cadence (the
// minEmitInterval floor rate-limits ticks, never phase visibility).
const phasePollInterval = 20 * time.Millisecond

// Emitter writes the -json NDJSON stream: a "start" snapshot when the run
// begins, an "update" snapshot on every interval tick plus immediately on
// each phase transition (never rate-limited), and one terminal
// "done"/"interrupted"/"error" snapshot exactly once at Stop. All output is
// stamped (v, tool, ts, elapsed_ms); the provider fills the counters.
//
// The emitter is fire-and-forget by design: a failed write (closed pipe, full
// disk) disables the stream silently and never disturbs the run.
type Emitter struct {
	w     io.Writer
	every time.Duration

	// mu guards the lifecycle fields below and serializes every write, so
	// concurrent Start/Stop callers can never interleave NDJSON lines.
	mu       sync.Mutex
	started  time.Time
	tool     string
	provider func(time.Time) Snapshot
	opened   bool // Start completed
	closed   bool // Stop completed; the stream is closed for good

	stop    chan struct{}
	stopped sync.WaitGroup
	once    sync.Once
	dead    atomic.Bool
	// lastPhase is the phase of the last emitted line (start included); the
	// loop compares each poll to detect phase transitions. Guarded by the
	// atomicity of atomic.Value; written by Start before the loop runs.
	lastPhase atomic.Value
}

// NewEmitter returns an emitter writing one JSON object per line to w every
// every. Intervals below minEmitInterval are clamped. Phase transitions are
// not rate-limited: a phase change emits an update immediately regardless of
// the interval, so a short phase (checking-history) is never invisible.
func NewEmitter(w io.Writer, every time.Duration) *Emitter {
	if every < minEmitInterval {
		every = minEmitInterval
	}
	return &Emitter{w: w, every: every, stop: make(chan struct{})}
}

// Start stamps and writes the first snapshot synchronously, then keeps
// emitting updates on the -json-every cadence and immediately on phase
// transitions until Stop. A Stop that completed before Start closes the
// stream permanently; Start after Stop (or a second Start) is a no-op. The
// provider must be safe to call from the emitter goroutine and must not set
// Event/V/Ts — those are owned here.
func (e *Emitter) Start(tool string, provider func(now time.Time) Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.opened || e.closed {
		return
	}
	e.opened = true
	e.started = time.Now()
	e.tool = tool
	e.provider = provider
	first := provider(time.Now())
	e.emitLocked(EventStart, first)
	if first.Phase != "" {
		e.lastPhase.Store(first.Phase)
	}
	e.stopped.Add(1)
	go e.loop()
}

func (e *Emitter) loop() {
	defer e.stopped.Done()
	poll := time.NewTicker(phasePollInterval)
	defer poll.Stop()
	lastUpdate := time.Now()
	for {
		select {
		case <-e.stop:
			return
		case now := <-poll.C:
			if e.dead.Load() {
				continue
			}
			s := e.provider(now)
			// Phase transitions are never rate-limited: each phase start is
			// visible as soon as it begins, even with -json-every far above
			// the floor (a checking-history phase that finishes under the
			// floor must still reach the stream).
			if s.Phase != "" && s.Phase != e.lastPhase.Load() {
				e.lastPhase.Store(s.Phase)
				e.emit(EventUpdate, s)
				lastUpdate = now
				continue
			}
			if now.Sub(lastUpdate) >= e.every {
				if s.Phase != "" {
					e.lastPhase.Store(s.Phase)
				}
				e.emit(EventUpdate, s)
				lastUpdate = now
			}
		}
	}
}

// emit stamps s and writes it as one NDJSON line.
func (e *Emitter) emit(event string, s Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emitLocked(event, s)
}

// emitLocked stamps s and writes it as one NDJSON line. Callers must hold
// e.mu. A write error marks the emitter dead; later emits (including Stop)
// become silent no-ops.
func (e *Emitter) emitLocked(event string, s Snapshot) {
	if e.dead.Load() {
		return
	}
	s.V = 1
	if s.Event == "" {
		s.Event = event
	}
	s.Tool = e.tool
	s.Ts = time.Now().Format(time.RFC3339Nano)
	s.ElapsedMS = time.Since(e.started).Milliseconds()
	b, err := json.Marshal(s)
	if err != nil {
		// A snapshot that cannot marshal is a programming bug; drop it and
		// keep the stream alive rather than corrupting the line framing.
		return
	}
	b = append(b, '\n')
	if _, werr := e.w.Write(b); werr != nil {
		e.dead.Store(true)
	}
}

// Stop writes the terminal snapshot exactly once, immediately followed by
// the final "summary" event — the very last line of the stream. The summary
// is the end-of-run rollup (see tuistat.SummaryBlock); a zero-value summary
// still emits a minimal identifiable {"event":"summary"} line, so an open
// stream always ends start → updates → terminal → summary. It is safe to
// call multiple times, concurrently with Start, and from any goroutine. A
// Stop that completes before Start closes the stream without any line:
// nothing was ever written, and a later Start is a no-op, so the stream
// stays permanently closed.
func (e *Emitter) Stop(final, summary Snapshot) {
	e.once.Do(func() {
		e.mu.Lock()
		if !e.opened {
			e.closed = true // permanent: a later Start must not open the stream
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()
		close(e.stop)
		e.stopped.Wait()
		e.emit("", final)
		// The summary line is forced to event=summary: callers fill blocks,
		// the emitter owns the framing (v/tool/ts/elapsed_ms envelope).
		summary.Event = EventSummary
		e.emit("", summary)
	})
}
