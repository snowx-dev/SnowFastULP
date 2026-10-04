package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
)

// defaultJSONEvery matches the -json-every flag default (800ms). The sfu/sfl
// variants live in cmd/sfu/jsonout.go and cmd/sfl/jsonout.go; all three
// binaries share the value.
const defaultJSONEvery = 800 * time.Millisecond

// jsonOut drives the -json NDJSON stream for one sfs run: a start
// snapshot, update ticks, and one terminal event followed by the final
// "summary" rollup. em is nil when the stream is off, so every method is a
// no-op. reg owns the force-exit stream close (nil in tests).
//
// The provider reads the run's live search.Metrics: main creates them before
// the stream opens, so the first snapshot already renders the index phase.
// Phases map onto the shared normalized vocabulary: indexing, searching,
// done.
type jsonOut struct {
	em  *tuistat.Emitter
	reg *termctl.RestoreRegistry // owns the force-exit stream close; nil in tests

	// The metrics exist from before start until process exit; the emitter
	// goroutine reads them on every tick, so the pointer is set once by
	// setMetrics before start and read via the atomic — never raced.
	mPtr atomic.Pointer[search.Metrics]

	// scan samples the active phase's byte counter (index bytes while
	// indexing, scanned bytes while searching). Own sampler: never shared
	// with the TUI monitor (which does not run under -json anyway).
	scan tuistat.RateSampler

	// providerForTest injects a panicking builder so the start-panic guard
	// stays testable.
	providerForTest func(now time.Time) tuistat.Snapshot
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
		// An early consumer exit (`sfs ... -json | head -1`) must not
		// kill the process mid-run: ignore SIGPIPE so the write returns
		// EPIPE and the emitter marks the side stream dead while the run
		// finishes.
		ignoreSIGPIPE()
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

// setMetrics hands the stream the run's live counters. Called exactly once,
// before start; the atomic pointer keeps the emitter goroutine's reads
// race-free.
func (j *jsonOut) setMetrics(m *search.Metrics) {
	j.mPtr.Store(m)
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
		// TrimSpace: the force-exit reason may open with "\n" for stderr
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
	j.em.Start("sfs", provider)
}

// snapshot is the production provider: the live search metrics rendered as a
// snapshot. m is never nil in production (main sets it before start); the
// nil branch keeps a history-less test start line identifiable.
func (j *jsonOut) snapshot(now time.Time) tuistat.Snapshot {
	m := j.mPtr.Load()
	if m == nil {
		return tuistat.Snapshot{Phase: tuistat.PhaseIndexing}
	}
	return sfsStatsSnapshot(m, j.phaseBPS(m, now))
}

// phaseBPS samples the active phase's byte counter: index bytes while
// indexing, scanned bytes while searching. Emitter-goroutine-only (same rule
// as the TUI monitor's samplers).
func (j *jsonOut) phaseBPS(m *search.Metrics, now time.Time) float64 {
	done, _ := phaseBytes(m)
	return j.scan.Rate(done, now)
}

// phaseBytes selects the byte counters the active phase reports: index bytes
// while indexing, scanned (decompressed) bytes while searching/done.
func phaseBytes(m *search.Metrics) (done, total int64) {
	if m.Phase.Load() == search.PhaseIndex {
		return indexBytes(m)
	}
	return searchBytes(m)
}

// sfsStatsSnapshot renders the live metrics as a snapshot, mirroring the
// fields the sfs TUI panel shows (archives, index/scanned bytes, chunks,
// hits). Blocks that carry no data are left nil so a cold run does not emit
// empty {} objects — same rule as the sfl/sfu providers.
func sfsStatsSnapshot(m *search.Metrics, bps float64) tuistat.Snapshot {
	var s tuistat.Snapshot
	switch m.Phase.Load() {
	case search.PhaseIndex:
		s.Phase = tuistat.PhaseIndexing
		s.Fraction = indexPercent(m)
	case search.PhaseSearch:
		s.Phase = tuistat.PhaseSearching
		s.Fraction = searchPercent(m)
	default:
		s.Phase = tuistat.PhaseDone
		s.Fraction = 1
	}
	done, total := phaseBytes(m)
	if done != 0 || total != 0 || bps != 0 {
		s.Bytes = &tuistat.BytesBlock{Read: done, Total: total, BPS: bps}
	}
	if m.Phase.Load() != search.PhaseIndex {
		// Chunk and hit progress only exist once the search began; the
		// indexing phase has neither, so both blocks stay nil there.
		chunkDone, chunkTotal := m.ChunksDone.Load(), m.ChunksTotal.Load()
		if chunkDone != 0 || chunkTotal != 0 {
			s.Chunks = &tuistat.DoneTotal{Done: chunkDone, Total: chunkTotal}
		}
		if hits := m.Hits.Load(); hits != 0 {
			s.Lines = &tuistat.LinesBlock{Hits: hits}
		}
	}
	if archTotal, archDone := m.ArchivesTotal.Load(), archiveProgress(m); archTotal != 0 || archDone != 0 {
		s.Sources = &tuistat.SourcesBlock{Archives: archTotal, ArchivesDone: archDone}
	}
	return s
}

// archiveProgress mirrors the TUI header: ArchivesIndexed while indexing,
// ArchivesDone from the search phase on.
func archiveProgress(m *search.Metrics) int64 {
	if m.Phase.Load() >= search.PhaseSearch {
		return m.ArchivesDone.Load()
	}
	return m.ArchivesIndexed.Load()
}

// stop writes the terminal snapshot exactly once, immediately followed by
// the final "summary" event carrying the end-of-run rollup. event selects
// the closing event; errMsg (when non-empty) rides the "error" field.
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
	// Sampler-free frame: stop runs on the main goroutine while the emitter
	// loop may still be mid-tick inside the provider, and the rate sampler
	// must stay emitter-goroutine-only.
	s := sfsStatsSnapshot(j.mPtr.Load(), 0)
	if event == tuistat.EventDone {
		s.Phase = tuistat.PhaseDone
		s.Fraction = 1
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

// summaryBlock builds the end-of-run rollup mirroring sfs's plain post-run
// summary (renderFinalSummary). It reuses the same lines/bytes/sources/chunks
// keys as the live and terminal snapshots (and as sfu/sfl summaries); the
// sfs-only truncation flag rides summary.truncated. Safe on a nil metrics
// pointer: an early exit before setMetrics simply omits the block.
func (j *jsonOut) summaryBlock() *tuistat.SummaryBlock {
	m := j.mPtr.Load()
	if m == nil {
		return nil
	}
	scannedDone, scannedTotal := searchBytes(m)
	b := &tuistat.SummaryBlock{
		Truncated: m.ChunksCapped.Load() > 0,
	}
	if hits := m.Hits.Load(); hits != 0 {
		b.Lines = &tuistat.LinesBlock{Hits: hits}
	}
	if scannedDone != 0 || scannedTotal != 0 {
		b.Bytes = &tuistat.BytesBlock{Read: scannedDone, Total: scannedTotal}
	}
	if archDone, archTotal := m.ArchivesDone.Load(), m.ArchivesTotal.Load(); archDone != 0 || archTotal != 0 {
		b.Sources = &tuistat.SourcesBlock{ArchivesDone: archDone, Archives: archTotal}
	}
	if chunkDone, chunkTotal := m.ChunksDone.Load(), m.ChunksTotal.Load(); chunkDone != 0 || chunkTotal != 0 {
		b.Chunks = &tuistat.DoneTotal{Done: chunkDone, Total: chunkTotal}
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
