package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// jsonOut drives the -json NDJSON stream for one sfl run: a start
// snapshot, update ticks, one terminal event, and a final "summary" event
// with the end-of-run rollup. Nil methods are no-ops, so callers never
// branch on whether -json was set.
type jsonOut struct {
	em   *tuistat.Emitter
	prog *sflog.Progress
	reg  *termctl.RestoreRegistry // owns the force-exit stream close; nil in tests
	bps  tuistat.RateSampler      // own sampler: never shares state with the TUI monitor

	// End-of-run rollup inputs, staged as the run reaches them and read by
	// stop() at close. Every stop site runs on the run goroutine; the mutex
	// only future-proofs a signal-goroutine stop.
	sumMu sync.Mutex
	sum   summaryData
}

// summaryData accumulates the final "summary" event's inputs: extraction
// stats, the committed output, and the -od ingest outcome. Zero fields are
// simply omitted by the summary builder.
type summaryData struct {
	stats     sflog.ExtractStats
	haveStats bool
	outPath   string
	libDir    string
	dryRun    bool
	ingestMet *ulpengine.Metrics
	ingestRes *ulpengine.Resolved
}

// setSummaryStats stages extraction stats once eng.Run returned; safe to
// call before ingest, so an ingest failure still summarizes what was parsed.
func (j *jsonOut) setSummaryStats(stats sflog.ExtractStats) {
	if j.em == nil {
		return
	}
	j.sumMu.Lock()
	j.sum.stats = stats
	j.sum.haveStats = true
	j.sumMu.Unlock()
}

// setSummaryTail stages the committed output path and the -od ingest
// outcome, called once the ingest block settled.
func (j *jsonOut) setSummaryTail(outPath string, ingestMet *ulpengine.Metrics, ingestRes *ulpengine.Resolved) {
	if j.em == nil {
		return
	}
	j.sumMu.Lock()
	j.sum.outPath = outPath
	j.sum.ingestMet = ingestMet
	j.sum.ingestRes = ingestRes
	j.sumMu.Unlock()
}

// newJSONOut resolves the -json target: "" = off, "-" = stdout, a path =
// a file created (truncated) for the stream. A file that cannot be opened is
// a usage error — an explicitly requested stream must not silently vanish.
// reg arms the force-exit stream close (may be nil in tests).
func newJSONOut(cfg runConfig, prog *sflog.Progress, reg *termctl.RestoreRegistry) (*jsonOut, error) {
	if cfg.JSONOut == "" {
		return &jsonOut{prog: prog, reg: reg}, nil
	}
	// The design requires a positive interval (the 200ms floor only applies
	// to positive values); anything else is a usage error, not a silent
	// fallback to the default. Validate before creating the file so a
	// rejected run leaves no truncated stream behind.
	if cfg.JSONEvery <= 0 {
		return nil, fmt.Errorf("invalid -json-every %v: must be positive", cfg.JSONEvery)
	}
	var w io.Writer
	if cfg.JSONOut == "-" && !cfg.JSONOutLiteral {
		// An early consumer exit (`sfl ... -json=- | head -1`) must not
		// kill the process mid-run: ignore SIGPIPE so the write returns EPIPE
		// and the emitter marks the side stream dead while the run finishes.
		ignoreSIGPIPE()
		w = os.Stdout
	} else {
		f, err := os.Create(cfg.JSONOut)
		if err != nil {
			return nil, fmt.Errorf("open -json file: %w", err)
		}
		w = f
	}
	return &jsonOut{em: tuistat.NewEmitter(w, cfg.JSONEvery), prog: prog, reg: reg,
		sum: summaryData{libDir: cfg.LibraryDir, dryRun: cfg.DryRun}}, nil
}

// start emits the initial snapshot and begins the update ticker. A panic
// during the synchronous start (snapshot builder or write) must still end
// the stream with an error terminal: the run's own panic recovery is armed
// only after start returns. The terminal is minimal — the possibly-panicking
// builder is not re-entered — and the panic re-surfaces with its stack.
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
	j.em.Start("sfl", func(now time.Time) tuistat.Snapshot {
		// The byte rate follows the active phase's counter: DoneBytes during
		// extract/ingest (the history check is sub-second under the sampled
		// fingerprint and carries no rate).
		byteRate := j.bps.Rate(j.prog.DoneBytes(), now)
		return j.prog.TUIStatSnapshot(byteRate, now)
	})
}

// stop writes the terminal snapshot exactly once, immediately followed by
// the final "summary" event carrying the end-of-run rollup. event selects
// the stream's closing event; errMsg (when non-empty) rides the "error"
// field.
func (j *jsonOut) stop(event, errMsg string) {
	j.stopWithCode(event, errMsg, 0)
}

// stopWithCode is stop with the exit code the run will end with: terminal
// "error" events carry it in the additive `code` field (internal/exitcode)
// so stream consumers can branch without parsing the message. All other
// events (and a zero code) omit it.
func (j *jsonOut) stopWithCode(event, errMsg string, code int) {
	if j.em == nil {
		return
	}
	var s tuistat.Snapshot
	if j.prog != nil {
		s = j.prog.TUIStatSnapshot(0, time.Now())
	}
	s.Event = event
	if event == tuistat.EventDone {
		s.Phase = tuistat.PhaseDone
		s.Status = "" // a done frame is terminal, never a mid-merge label
		s.Fraction = 1
	}
	if errMsg != "" {
		s.Error = errMsg
	}
	if event == tuistat.EventError && code != 0 {
		s.Code = code
	}
	j.em.Stop(s, tuistat.Snapshot{Summary: j.summaryBlock()})
	// Disarm the force-exit hook: the stream is closed for good. The
	// emitter's once-guard already makes a stale hook call harmless; this
	// keeps the registry clean so a later ForceExit cannot touch the stream.
	if j.reg != nil {
		j.reg.ClearExitHook()
	}
}

// summaryBlock builds the end-of-run rollup mirroring sfl's human recap
// (recapCountRows + the ingest Added/Removed rows + path footers): line
// totals with the per-reason drop breakdown, source counts, history, env,
// the -od library outcome, and committed output paths. Early exits simply
// omit what never happened.
func (j *jsonOut) summaryBlock() *tuistat.SummaryBlock {
	j.sumMu.Lock()
	d := j.sum
	j.sumMu.Unlock()
	b := &tuistat.SummaryBlock{}
	if d.haveStats {
		st := d.stats
		// sfl parses credentials, not raw lines: read totals are the
		// parser's business, so the block carries parsed/unique/dupes only.
		b.Lines = &tuistat.LinesBlock{
			Accepted: int64(st.Credentials),
			Unique:   int64(st.Emitted),
			Dupes:    int64(st.Duplicates),
		}
		b.Sources = &tuistat.SourcesBlock{
			Files:    int64(st.FilesScanned),
			Archives: int64(st.ArchivesScanned),
			LogsDone: int64(st.Logs),
			Skipped:  int64(st.SkippedFiles + st.SkippedArchives),
		}
		if st.HistoryChecked > 0 || st.HistorySkipped > 0 {
			b.History = &tuistat.HistoryBlock{
				Enabled: true,
				Checked: int64(st.HistoryChecked), Skipped: int64(st.HistorySkipped),
			}
		}
		if st.EnvCopied > 0 || st.EnvDeduped > 0 || st.EnvDirsCopied > 0 ||
			st.EnvSkippedOverCap > 0 || st.EnvDirsSkippedOverCap > 0 {
			b.Env = &tuistat.EnvBlock{
				Enabled:            true,
				Copied:             int64(st.EnvCopied),
				Deduped:            int64(st.EnvDeduped),
				DirsCopied:         int64(st.EnvDirsCopied),
				SkippedOverCap:     int64(st.EnvSkippedOverCap),
				DirsSkippedOverCap: int64(st.EnvDirsSkippedOverCap),
			}
		}
	}
	if j.prog != nil {
		if rd := j.prog.DoneBytes(); rd != 0 {
			b.Bytes = &tuistat.BytesBlock{Read: rd}
		}
	}
	rej := &tuistat.SummaryRejects{}
	if d.haveStats {
		rej.Dupes = int64(d.stats.Duplicates)
	}
	if d.ingestMet != nil {
		// Same mapping the ingest recap rows use: Added = unique lines the
		// engine wrote, already-in-library hits and library-refused creds
		// close the arithmetic against extraction's unique count.
		rej.InLibrary = d.ingestMet.LinesSkippedByDest.Load()
		rej.LibraryRefused = d.ingestMet.LinesRejected.Load()
		b.Library = &tuistat.SummaryLibrary{
			Lines: ingestLibraryLines(d.ingestRes, d.ingestMet),
			Added: d.ingestMet.LinesUnique.Load(),
		}
	}
	rej.Total = rej.Dupes + rej.InLibrary + rej.LibraryRefused
	if rej.Total != 0 || rej.TooLong != 0 || rej.Malformed != 0 || rej.Unrepresentable != 0 {
		b.Rejects = rej
	}
	var paths []string
	if d.libDir != "" || d.ingestRes != nil {
		// -od/-odr: the library dir plus the ingest's committed paths (dry
		// runs write nothing, so only the library dir is reported).
		if d.libDir != "" {
			paths = append(paths, d.libDir)
		}
		if !d.dryRun {
			paths = append(paths, ingestOutputPaths(d.ingestRes)...)
		}
	} else if d.haveStats && d.stats.Emitted > 0 && d.outPath != "" {
		paths = append(paths, d.outPath)
	}
	if len(paths) > 0 {
		b.Output = &tuistat.OutputBlock{Paths: paths}
	}
	return b
}

// defaultJSONEvery matches the -json-every flag default (the user chose 800ms).
const defaultJSONEvery = 800 * time.Millisecond

// jsonOutTarget converts a parsed OutTarget flag into runConfig fields:
// enabled stdout is "-", a file is its path, disabled is "".
func jsonOutTarget(t *cliargs.OutTarget) string {
	if !t.Enabled {
		return ""
	}
	if t.Stdout() {
		return "-"
	}
	return t.Path
}
