package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// defaultJSONEvery matches the -json-every flag default (800ms). The sfl
// variant lives in cmd/sfl/jsonout.go; both binaries share the value.
const defaultJSONEvery = 800 * time.Millisecond

// jsonOut drives the -json NDJSON stream for one sfu run: a start
// snapshot, update ticks, and one terminal event. em is nil when the stream
// is off, so every method is a no-op. reg owns the force-exit stream close
// (nil in tests).
//
// With -history the stream opens before the history fingerprinting, so the
// run's first phase is checking-history: while the engine objects don't
// exist yet the provider reads the live historyCounters directly; once
// setEngine runs the provider switches to the engine's StatsSnapshot.
type jsonOut struct {
	em  *tuistat.Emitter
	hc  *historyCounters         // live checking-history counters; nil without -history
	reg *termctl.RestoreRegistry // owns the force-exit stream close; nil in tests

	// The engine objects appear mid-run (after Resolve), while the emitter
	// goroutine reads them on every tick: set once by setEngine, read via
	// the atomics, never raced.
	mPtr atomic.Pointer[ulpengine.Metrics]
	rPtr atomic.Pointer[ulpengine.Resolved]

	read  tuistat.RateSampler // own samplers: never shared with the TUI monitor
	shard tuistat.RateSampler
	write tuistat.RateSampler
	regen tuistat.RateSampler

	// providerForTest injects a panicking builder so the start-panic guard
	// stays testable now that the production provider is nil-safe.
	providerForTest func(now time.Time) tuistat.Snapshot
}

// historyCounters carries the live checking-history progress the -json
// provider reads before the engine objects exist. All fields are atomics:
// FingerprintAll's progress callback fires concurrently from the prehash
// workers, and the emitter goroutine reads on every tick.
type historyCounters struct {
	bytesDone  atomic.Int64
	bytesTotal atomic.Int64
	checked    atomic.Int64
	// filesTotal feeds the inline checking bar's "N/M files" segment; the
	// JSON history block has no key for it (checked is the live file count).
	filesTotal atomic.Int64
	skipped    atomic.Int64
}

// record folds one FingerprintAll progress callback into the counters.
// FingerprintAll calls it concurrently from its workers; the aggregates it
// delivers are monotonic in the order the workers' atomic Adds ran, but the
// deliveries themselves are not ordered, so each field keeps the maximum
// ever seen — a stale delivery can never move a counter backwards (which
// would surface downstream as a regressing bytes_done and a negative bps).
func (h *historyCounters) record(filesDone, filesTotal int, bytesDone, bytesTotal int64) {
	h.keepMax(&h.bytesDone, bytesDone)
	h.keepMax(&h.bytesTotal, bytesTotal)
	h.keepMax(&h.checked, int64(filesDone))
	h.keepMax(&h.filesTotal, int64(filesTotal))
}

// keepMax stores v into dst unless a concurrent writer already stored more.
func (h *historyCounters) keepMax(dst *atomic.Int64, v int64) {
	for {
		cur := dst.Load()
		if v <= cur || dst.CompareAndSwap(cur, v) {
			return
		}
	}
}

// setSkipped stages the history-database tally once Select resolved which
// sources are already completed.
func (h *historyCounters) setSkipped(n int) {
	h.skipped.Store(int64(n))
}

// newJSONOut resolves the -json target: "" = off, "-" = stdout, a path =
// a file created (truncated) for the stream. A file that cannot be opened is
// an error — an explicitly requested stream must not silently vanish. reg
// arms the force-exit stream close (may be nil in tests).
func newJSONOut(target string, every time.Duration, reg *termctl.RestoreRegistry) (*jsonOut, error) {
	j := &jsonOut{reg: reg}
	if target == "" {
		return j, nil
	}
	// The design requires a positive interval (the 200ms floor only applies
	// to positive values); anything else is a usage error, not a silent
	// fallback to the default. Validate before creating the file so a
	// rejected run leaves no truncated stream behind.
	if every <= 0 {
		return nil, fmt.Errorf("invalid -json-every %v: must be positive", every)
	}
	var w io.Writer
	if target == "-" {
		w = os.Stdout
	} else {
		f, err := os.Create(target)
		if err != nil {
			return nil, fmt.Errorf("open -json file: %w", err)
		}
		w = f
	}
	j.em = tuistat.NewEmitter(w, every)
	return j, nil
}

// setEngine hands the stream the resolved engine objects; the provider
// switches from the history phase to the engine phases on the next tick.
// Safe to call exactly once while the emitter is running: the atomics keep
// the emitter goroutine's reads race-free.
func (j *jsonOut) setEngine(m *ulpengine.Metrics, r *ulpengine.Resolved) {
	j.mPtr.Store(m)
	j.rPtr.Store(r)
}

// start emits the initial snapshot and begins the update ticker. A panic
// during the synchronous start (snapshot builder or write) must still end
// the stream with an error terminal: main's recover hook reads joutRef,
// which is assigned only after start returns. The terminal is minimal — the
// possibly-panicking builder is not re-entered — and the panic re-surfaces.
func (j *jsonOut) start() {
	if j.em == nil {
		return
	}
	// Arm the force-exit hook before the stream runs: a second Ctrl-C or a
	// cleanup-timeout force-exit fires on the signal/watcher goroutine, where
	// the deferred stop never gets to run, and without this the stream would
	// die on bare update lines. stop() disarms the hook once the stream has
	// closed for good.
	if j.reg != nil {
		// TrimSpace: the cleanup-timeout reason opens with "\n" for stderr
		// formatting; the JSON error field should not carry it.
		j.reg.SetExitHook(func(reason string) { j.stop(tuistat.EventInterrupted, strings.TrimSpace(reason)) })
	}
	defer func() {
		if r := recover(); r != nil {
			j.em.Stop(tuistat.Snapshot{
				Event: tuistat.EventError,
				Error: fmt.Sprintf("panic: %v", r),
			}, tuistat.Snapshot{})
			panic(r)
		}
	}()
	provider := j.snapshot
	if j.providerForTest != nil {
		provider = j.providerForTest
	}
	j.em.Start("sfu", provider)
}

// snapshot is the production provider: the history counters while the engine
// is unresolved (nil-safe — with -history the stream opens before the engine
// exists), the engine's StatsSnapshot from there on. Without -history main
// hands the engine over before start, so the pre-engine branch never renders
// a history-less run.
func (j *jsonOut) snapshot(now time.Time) tuistat.Snapshot {
	m, r := j.mPtr.Load(), j.rPtr.Load()
	if m == nil || r == nil {
		return j.historySnapshot()
	}
	return ulpengine.StatsSnapshot(m, r, j.rates(m, r, now), now)
}

// historyFrame renders the checking-history counters as a snapshot. The
// sampled fingerprint completes the phase near-instantly, so no throughput
// is sampled or reported for it.
func (j *jsonOut) historyFrame() tuistat.Snapshot {
	s := tuistat.Snapshot{Phase: tuistat.PhaseCheckingHistory}
	if j.hc == nil {
		return s
	}
	done := j.hc.bytesDone.Load()
	total := j.hc.bytesTotal.Load()
	s.History = &tuistat.HistoryBlock{
		Enabled:    true,
		BytesDone:  done,
		BytesTotal: total,
		Checked:    j.hc.checked.Load(),
		Skipped:    j.hc.skipped.Load(),
	}
	if total > 0 {
		s.Fraction = min(1, float64(done)/float64(total))
	}
	return s
}

// historySnapshot renders the checking-history phase from the live counters.
func (j *jsonOut) historySnapshot() tuistat.Snapshot {
	return j.historyFrame()
}

// rates samples the per-phase throughputs the engine exposes.
func (j *jsonOut) rates(m *ulpengine.Metrics, r *ulpengine.Resolved, now time.Time) tuistat.Rates {
	return tuistat.Rates{
		Read:  j.read.Rate(m.BytesRead.Load(), now),
		Shard: j.shard.Rate(m.BytesShard.Load(), now),
		Write: j.write.Rate(m.BytesWritten.Load(), now),
		Regen: j.regenRate(r, now),
	}
}

// regenRate samples the OD regen counter when -od is active.
func (j *jsonOut) regenRate(r *ulpengine.Resolved, now time.Time) float64 {
	if r.OdMetrics == nil {
		return 0
	}
	return j.regen.Rate(r.OdMetrics.RegenBytesRead.Load(), now)
}

// stop writes the terminal snapshot exactly once, immediately followed by
// the final "summary" event carrying the end-of-run rollup. event selects
// the closing event; errMsg (when non-empty) rides the "error" field.
// If the engine never resolved (interrupt/error mid-history check), the
// terminal keeps the checking-history phase; a done (history-only run)
// reports phase done.
func (j *jsonOut) stop(event, errMsg string) {
	j.stopWithCode(event, errMsg, 0)
}

// stopWithCode is stop with the exit code the process will end with: terminal
// "error" events carry it in the additive `code` field (internal/exitcode) so
// stream consumers can branch without parsing the message. All other events
// (and a zero code) omit it.
func (j *jsonOut) stopWithCode(event, errMsg string, code int) {
	if j.em == nil {
		return
	}
	m, r := j.mPtr.Load(), j.rPtr.Load()
	var s tuistat.Snapshot
	if m == nil || r == nil {
		// Sampler-free frame: stop runs on the main goroutine while the
		// emitter loop may still be mid-tick inside the provider, and the
		// byte-rate sampler must stay emitter-goroutine-only.
		s = j.historyFrame()
		if event == tuistat.EventDone {
			s.Phase = tuistat.PhaseDone
			s.Fraction = 1
		}
	} else {
		s = ulpengine.StatsSnapshot(m, r, tuistat.Rates{}, time.Now())
		if event == tuistat.EventDone {
			s.Phase = tuistat.PhaseDone
			s.Status = "" // a done frame is terminal, never a mid-merge label
		}
	}
	if errMsg != "" {
		s.Error = errMsg
	}
	if event == tuistat.EventError && code != 0 {
		s.Code = code
	}
	s.Event = event
	j.em.Stop(s, tuistat.Snapshot{Summary: j.summaryBlock()})
	// Disarm the force-exit hook: the stream is closed for good. The
	// emitter's once-guard already makes a stale hook call harmless; this
	// keeps the registry clean so a later ForceExit cannot touch the stream.
	if j.reg != nil {
		j.reg.ClearExitHook()
	}
}

// summaryBlock builds the end-of-run rollup mirroring sfu's human DONE
// recap (renderDoneLines + renderODSummary): line totals with the per-reason
// reject/drop breakdown, input/output bytes, file counts, history, -od
// library outcome, and committed output paths. Safe on partially-run or nil
// engines: a history-only run (engine never resolved) summarizes the history
// tallies alone, and earlier exits simply omit what never happened.
func (j *jsonOut) summaryBlock() *tuistat.SummaryBlock {
	m, r := j.mPtr.Load(), j.rPtr.Load()
	if m == nil || r == nil {
		// History-only run (or engine never resolved): the rollup is the
		// history tally — checked sources and how many the database
		// reported as already completed.
		if j.hc == nil {
			return nil
		}
		return &tuistat.SummaryBlock{
			History: &tuistat.HistoryBlock{
				Enabled: true,
				Checked: j.hc.checked.Load(),
				Skipped: j.hc.skipped.Load(),
			},
		}
	}
	read := m.LinesRead.Load()
	accepted := m.LinesAccepted.Load()
	rejected := m.LinesRejected.Load()
	unique := m.LinesUnique.Load()
	inLib := m.LinesSkippedByDest.Load()
	unrep := m.LinesUnrepresentable.Load()
	// genuine within-run dups, same math as renderDoneLines: parsed cleanly
	// minus unique minus library hits.
	dup := accepted - unique - inLib
	if dup < 0 {
		dup = 0
	}
	b := &tuistat.SummaryBlock{
		Lines: &tuistat.LinesBlock{
			Read:            read,
			Accepted:        accepted,
			Rejected:        rejected,
			Unique:          unique,
			Dupes:           dup,
			InLibrary:       inLib,
			Unrepresentable: unrep,
		},
	}
	if rej := (&tuistat.SummaryRejects{
		Total:           rejected + dup + inLib,
		Dupes:           dup,
		InLibrary:       inLib,
		Unrepresentable: unrep,
		TooLong:         m.LinesTooLong.Load(),
		Malformed:       m.LinesMalformed.Load(),
		PasswordTooLong: m.LinesPasswordTooLong.Load(),
	}); rej.Total != 0 || rej.TooLong != 0 || rej.Malformed != 0 || rej.PasswordTooLong != 0 || rej.Unrepresentable != 0 {
		b.Rejects = rej
	}
	if rd, wr := m.BytesRead.Load(), m.BytesWritten.Load(); rd != 0 || wr != 0 {
		b.Bytes = &tuistat.BytesBlock{Read: rd, Written: wr}
	}
	if r.InputFileCount != 0 {
		b.Sources = &tuistat.SourcesBlock{
			Files:   int64(r.InputFileCount),
			Deleted: int64(len(r.DeletedInputPaths)),
		}
	}
	if r.HistoryChecked > 0 || r.HistorySkipped > 0 {
		b.History = &tuistat.HistoryBlock{
			Enabled: true,
			Checked: int64(r.HistoryChecked), Skipped: int64(r.HistorySkipped),
		}
	}
	if r.OdResult != nil {
		// The -od recap's "lines in library" total; in a dry run the total is
		// the unchanged pre-run count (same rule as libraryLineCountTotal),
		// and Added is the would-add count the -odr preview exists to show.
		lib := &tuistat.SummaryLibrary{
			Lines:         libraryLineCountTotal(r, m),
			Added:         unique,
			UpgradedParts: int32(r.OdResult.ArchivesUpgraded),
		}
		b.Library = lib
	}
	if !r.Cfg.DryRun && len(r.OutputPaths) > 0 {
		b.Output = &tuistat.OutputBlock{Paths: r.OutputPaths}
	}
	return b
}

// jsonOutTarget converts a parsed OutTarget flag into a stream target:
// disabled = "", stdout = "-", a file = its path.
func jsonOutTarget(t *cliargs.OutTarget) string {
	if !t.Enabled {
		return ""
	}
	if t.Stdout() {
		return "-"
	}
	return t.Path
}
