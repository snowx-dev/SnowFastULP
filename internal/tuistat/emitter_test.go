package tuistat

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// decodeLines splits an NDJSON stream and unmarshals every line.
func decodeLines(t *testing.T, buf string) []Snapshot {
	t.Helper()
	var out []Snapshot
	for _, ln := range strings.Split(strings.TrimSpace(buf), "\n") {
		if ln == "" {
			continue
		}
		var s Snapshot
		if err := json.Unmarshal([]byte(ln), &s); err != nil {
			t.Fatalf("line %q is not valid snapshot JSON: %v", ln, err)
		}
		out = append(out, s)
	}
	return out
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(deadline time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func constProvider(bytesRead int64) func(time.Time) Snapshot {
	return func(time.Time) Snapshot {
		return Snapshot{Tool: "ignored", Phase: "parsing", Bytes: &BytesBlock{Read: bytesRead}}
	}
}

func TestEmitterStartEmitsStartThenUpdates(t *testing.T) {
	var buf lockedBuffer
	e := NewEmitter(&buf, 25*time.Millisecond)
	e.Start("sfu", constProvider(42))

	// First line is written synchronously by Start: always present.
	first := decodeLines(t, buf.String())
	if len(first) != 1 {
		t.Fatalf("after Start: %d lines, want exactly the start line: %q", len(first), buf.String())
	}
	if first[0].Event != EventStart {
		t.Fatalf("first event = %q, want %q", first[0].Event, EventStart)
	}
	if first[0].V != 1 {
		t.Fatalf("first v = %d, want 1", first[0].V)
	}
	if first[0].Tool != "sfu" {
		t.Fatalf("tool = %q, want sfu", first[0].Tool)
	}
	if first[0].Ts == "" {
		t.Fatal("start snapshot has no ts")
	}
	if first[0].Bytes == nil || first[0].Bytes.Read != 42 {
		t.Fatalf("provider data lost: %+v", first[0].Bytes)
	}
	if first[0].Phase != "parsing" {
		t.Fatalf("phase = %q, want parsing", first[0].Phase)
	}

	// Ticker keeps emitting updates with fresh data.
	if !waitFor(2*time.Second, func() bool { return strings.Count(buf.String(), "\n") >= 2 }) {
		t.Fatalf("no update ticks within 2s: %q", buf.String())
	}
	e.Stop(Snapshot{Event: EventDone, Phase: "done"}, Snapshot{})
	e.Stop(Snapshot{Event: EventDone, Phase: "done"}, Snapshot{}) // idempotent

	snaps := decodeLines(t, buf.String())
	last := snaps[len(snaps)-1]
	if last.Event != EventSummary {
		t.Fatalf("final event = %q, want %q (summary follows the terminal)", last.Event, EventSummary)
	}
	terminal := snaps[len(snaps)-2]
	if terminal.Event != EventDone {
		t.Fatalf("terminal event = %q, want done", terminal.Event)
	}
	if terminal.Phase != "done" {
		t.Fatalf("terminal phase = %q, want done", terminal.Phase)
	}
	for _, s := range snaps {
		if s.Tool != "sfu" {
			t.Fatalf("tool not stamped on %q event: %+v", s.Event, s)
		}
		if s.Ts == "" {
			t.Fatalf("ts not stamped on %q event", s.Event)
		}
	}
	// Two Stop calls must not duplicate the terminal or summary lines.
	var doneCount, summaryCount int
	for _, s := range snaps {
		switch s.Event {
		case EventDone:
			doneCount++
		case EventSummary:
			summaryCount++
		}
	}
	if doneCount != 1 {
		t.Fatalf("terminal event emitted %d times, want exactly 1", doneCount)
	}
	if summaryCount != 1 {
		t.Fatalf("summary event emitted %d times, want exactly 1", summaryCount)
	}
}

func TestEmitterUpdateEventStampedByEmitter(t *testing.T) {
	var buf lockedBuffer
	e := NewEmitter(&buf, 15*time.Millisecond)
	// Provider forgets to set Event: the emitter stamps "update" itself.
	e.Start("sfl", func(time.Time) Snapshot { return Snapshot{Phase: "extracting"} })
	if !waitFor(2*time.Second, func() bool { return strings.Count(buf.String(), "\n") >= 3 }) {
		t.Fatalf("expected start + 2 updates, got: %q", buf.String())
	}
	e.Stop(Snapshot{Event: EventDone}, Snapshot{})
	snaps := decodeLines(t, buf.String())
	if len(snaps) < 3 {
		t.Fatalf("want start + >=2 updates, got %d", len(snaps))
	}
	if snaps[1].Event != EventUpdate || snaps[2].Event != EventUpdate {
		t.Fatalf("tick events = %q, %q, want update/update", snaps[1].Event, snaps[2].Event)
	}
}

func TestEmitterStopCarriesTerminalData(t *testing.T) {
	var buf lockedBuffer
	e := NewEmitter(&buf, time.Hour)
	e.Start("sfu", constProvider(0))
	e.Stop(Snapshot{Event: EventError, Phase: "done", Error: "disk full"},
		Snapshot{Summary: &SummaryBlock{Lines: &LinesBlock{Unique: 7}}})
	snaps := decodeLines(t, buf.String())
	if len(snaps) != 3 {
		t.Fatalf("want start + error + summary, got %d lines: %q", len(snaps), buf.String())
	}
	if snaps[1].Event != EventError || snaps[1].Error != "disk full" {
		t.Fatalf("terminal snapshot lost data: %+v", snaps[1])
	}
	// The summary line follows the terminal, is identifiable, and carries
	// the rollup; its elapsed_ms is stamped from the emitter's own start.
	if snaps[2].Event != EventSummary {
		t.Fatalf("last event = %q, want %q", snaps[2].Event, EventSummary)
	}
	if snaps[2].Summary == nil || snaps[2].Summary.Lines == nil || snaps[2].Summary.Lines.Unique != 7 {
		t.Fatalf("summary rollup lost data: %+v", snaps[2].Summary)
	}
}

// The summary event is the very last line of every open stream, even a
// zero-value one: an interrupted or panicking run still ends with a single
// identifiable summary line after the terminal.
func TestEmitterStopEmitsExactlyOneSummaryLine(t *testing.T) {
	var buf lockedBuffer
	e := NewEmitter(&buf, time.Hour)
	e.Start("sfl", constProvider(0))
	e.Stop(Snapshot{Event: EventInterrupted}, Snapshot{})
	e.Stop(Snapshot{Event: EventDone}, Snapshot{Summary: &SummaryBlock{Lines: &LinesBlock{Unique: 9}}})
	snaps := decodeLines(t, buf.String())
	if len(snaps) != 3 {
		t.Fatalf("want start + terminal + summary, got %d: %q", len(snaps), buf.String())
	}
	if snaps[1].Event != EventInterrupted {
		t.Fatalf("terminal = %q, want interrupted (exactly-once)", snaps[1].Event)
	}
	if snaps[2].Event != EventSummary {
		t.Fatalf("last = %q, want summary", snaps[2].Event)
	}
	// Exactly-once: the FIRST Stop owns both lines, so the summary is the
	// zero-value rollup from that call — the second Stop's payload is
	// dropped along with its duplicate terminal.
	if snaps[2].Summary != nil {
		t.Fatalf("second Stop must be a no-op, got rollup %+v", snaps[2].Summary)
	}
}

func TestEmitterWriteErrorDisablesSilently(t *testing.T) {
	w := &failAfterWriter{failAfter: 2} // start + one update, then fail
	e := NewEmitter(w, 10*time.Millisecond)
	e.Start("sfu", constProvider(1))
	if !waitFor(2*time.Second, func() bool { return w.failed.Load() }) {
		t.Fatal("emitter never hit the write error")
	}
	before := strings.Count(w.String(), "\n")
	if before < 2 {
		t.Fatalf("write error fired before start+update completed (%d lines): test setup broken", before)
	}
	e.Stop(Snapshot{Event: EventDone}, Snapshot{})
	after := strings.Count(w.String(), "\n")
	if after != before {
		t.Fatalf("Stop wrote after a failed write (%d -> %d lines): terminal emit must be disabled by a dead writer", before, after)
	}
}

func TestEmitterIntervalFloor(t *testing.T) {
	if got := NewEmitter(&lockedBuffer{}, time.Millisecond).every; got < minEmitInterval {
		t.Fatalf("interval = %v, want clamped to >= %v", got, minEmitInterval)
	}
	if got := NewEmitter(&lockedBuffer{}, 800*time.Millisecond).every; got != 800*time.Millisecond {
		t.Fatalf("interval = %v, want 800ms unchanged", got)
	}
}

// A Stop that completes before Start closes the stream permanently: nothing
// is written and a later Start is a no-op (the start line could never get a
// terminal).
func TestEmitterStopBeforeStartStaysClosed(t *testing.T) {
	var buf lockedBuffer
	e := NewEmitter(&buf, 15*time.Millisecond)
	e.Stop(Snapshot{Event: EventDone, Phase: "done"}, Snapshot{})
	e.Start("sfu", constProvider(1)) // must be a no-op
	if got := buf.String(); got != "" {
		t.Fatalf("stream wrote %q after Stop-before-Start, want permanently silent", got)
	}
}

// Concurrent Start/Stop may pick either ordering, but the NDJSON framing must
// never interleave: every line stays a complete JSON object, and when the
// stream opened the summary line is the very last one.
func TestEmitterConcurrentStartStopKeepsFraming(t *testing.T) {
	for range 20 {
		var buf lockedBuffer
		e := NewEmitter(&buf, 5*time.Millisecond)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); e.Start("sfu", constProvider(1)) }()
		go func() { defer wg.Done(); e.Stop(Snapshot{Event: EventDone, Phase: "done"}, Snapshot{}) }()
		wg.Wait()
		snaps := decodeLines(t, buf.String())
		for _, s := range snaps {
			if s.Tool != "sfu" {
				t.Fatalf("line missing tool stamp: %+v", s)
			}
		}
		if len(snaps) > 0 && snaps[len(snaps)-1].Event != EventSummary {
			t.Fatalf("open stream must end in summary, got %q", snaps[len(snaps)-1].Event)
		}
	}
}

// --- helpers ---

// lockedBuffer is a mutex-guarded bytes.Buffer: the emitter goroutine writes
// while the test reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// failAfterWriter fails writes after n successful ones, records the bytes it
// accepted, and reports the failure so tests can wait for it.
type failAfterWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	n         int
	failAfter int
	failed    atomic.Bool
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n >= f.failAfter {
		f.failed.Store(true)
		return 0, errors.New("pipe closed")
	}
	f.n++
	f.buf.Write(p)
	return len(p), nil
}
func (f *failAfterWriter) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// Phase transitions must emit immediately, above the floor: with a -json-every
// far above minEmitInterval, a phase change still reaches the stream within a
// poll window — a short checking-history phase is never invisible.
func TestEmitterPhaseChangeEmitsAboveFloor(t *testing.T) {
	var buf lockedBuffer
	e := NewEmitter(&buf, 10*time.Second)
	phase := atomic.Value{}
	phase.Store("checking-history")
	e.Start("sfu", func(time.Time) Snapshot {
		return Snapshot{Phase: phase.Load().(string)}
	})
	// Transition after Start: must produce an update line well within the
	// 10s interval (phasePollInterval is 20ms; allow scheduler slack).
	phase.Store("extracting")
	if !waitFor(2*time.Second, func() bool {
		for _, s := range decodeLines(t, buf.String()) {
			if s.Event == EventUpdate && s.Phase == "extracting" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("phase change never emitted an update within 2s: %q", buf.String())
	}
	// No within-phase ticks may appear between start and the phase event:
	// 10s has not elapsed.
	lines := decodeLines(t, buf.String())
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want exactly start + phase-change update: %q", len(lines), buf.String())
	}
	e.Stop(Snapshot{Event: EventDone, Phase: "done"}, Snapshot{})
}

// A second transition (a new phase after a phase event) still emits: the
// last-phase tracking compares against the most recent emitted line.
func TestEmitterSecondPhaseTransitionEmits(t *testing.T) {
	var buf lockedBuffer
	e := NewEmitter(&buf, 10*time.Second)
	phase := atomic.Value{}
	phase.Store("extracting")
	e.Start("sfu", func(time.Time) Snapshot { return Snapshot{Phase: phase.Load().(string)} })
	phase.Store("done")
	if !waitFor(2*time.Second, func() bool {
		for _, s := range decodeLines(t, buf.String()) {
			if s.Event == EventUpdate && s.Phase == "done" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("second phase transition never emitted: %q", buf.String())
	}
	e.Stop(Snapshot{Event: EventDone, Phase: "done"}, Snapshot{})
}
