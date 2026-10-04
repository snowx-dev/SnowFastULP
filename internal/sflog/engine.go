package sflog

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// errPasswordNotFound marks an archive that no password candidate could open.
var errPasswordNotFound = errors.New("password not found")

// errMissingFirstVolume marks an orphaned multi-volume RAR continuation part
// (e.g. a stray name.part2.rar with no name.part1.rar present).
var errMissingFirstVolume = errors.New("first volume of the set not found (name.part1.rar / .part01.rar)")

// callbackGuard converts a panic inside an integration callback (Engine Debug,
// Engine OnIssue, EnvCopier error handler) into a run error. Untrusted hooks
// must never take down a worker goroutine: the first panic is recorded once as
// a wrapped error and the cancel func fires so the run stops producing output;
// every later callback invocation becomes a no-op. A non-nil error is a RUN
// failure — never an archive parse issue — so callers keep all sources out of
// -del.
type callbackGuard struct {
	mu     sync.Mutex
	runErr error
	cancel func()
}

func newCallbackGuard(cancel func()) *callbackGuard {
	return &callbackGuard{cancel: cancel}
}

// failed reports whether a callback already panicked.
func (g *callbackGuard) failed() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runErr != nil
}

// callbackErr returns the recorded callback panic error, or nil.
func (g *callbackGuard) callbackErr() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runErr
}

// run invokes fn under panic recovery. When the guard already failed, fn is
// not invoked at all. The first panic records "sflog: <name> callback panic:
// <value>" and fires the cancel func.
func (g *callbackGuard) run(name string, fn func()) {
	if g == nil || fn == nil || g.failed() {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			g.mu.Lock()
			if g.runErr == nil {
				g.runErr = fmt.Errorf("sflog: %s callback panic: %v", name, r)
				if g.cancel != nil {
					g.cancel()
				}
			}
			g.mu.Unlock()
		}
	}()
	fn()
}

// perKindIssueCap bounds how many concrete example paths we keep per issue kind
// for the summary; the integer counters stay exact regardless. Keeping a budget
// per kind ensures important kinds (e.g. password-not-found) always get
// examples even when another kind is far more frequent.
const perKindIssueCap = 10

// Engine streams credentials from a discovered worklist through a worker pool
// into a single fan-in writer that deduplicates and writes ULP lines. Memory is
// bounded: workers parse in parallel, the writer keeps only a uint64 hash set.
type Engine struct {
	Workers   int
	NoURI     bool
	Loose     bool // select the shared high-recall parser for raw and emitted records
	Passwords []string
	// winner is the run-shared sticky password slot: the last password that
	// opened an archive is tried first by every later archive. Zero value is
	// ready to use; shared by all workers through extractCtx.winner.
	winner   runWinner
	Progress *Progress
	Debug    func(format string, args ...any)
	// OnIssue, when set, is called once for every issue as it happens — before
	// the summary's per-kind cap — so a caller can stream a complete,
	// untruncated issue log. Invoked concurrently from workers; the sink guards
	// its own state.
	OnIssue func(path string, kind IssueKind, err error)
	// DedupKey (optional) maps a formatted ULP line to the canonical library
	// dedup key (host:login:password). When set, the writer dedups on it instead
	// of the whole line, so sfl's "unique" count means the same thing the library
	// (and sfu) mean — path-only variants of the same credential collapse here
	// instead of surviving extraction only to be merged at ingest. nil hashes the
	// whole line (path-sensitive), used by direct/test callers with no library.
	DedupKey func(line string) (uint64, bool)
	// TempDir is where nested archive members are spilled before being recursed
	// into. "" falls back to the system temp dir.
	TempDir string
	// EnvCopier (optional) copies allowlisted env/key files to a side directory.
	// nil disables -env entirely.
	EnvCopier *EnvCopier
	EnvMaxLen int64
	// FollowedByIngest tells Run to leave the tracker in the extract phase
	// instead of flipping to Done, so an -od caller can hand straight off to the
	// ingest phase without a transient "COMPLETE" frame.
	FollowedByIngest bool
	History          history.Store

	// extractSem is the engine-wide extraction budget (cap = worker count),
	// allocated in Run. Every worker holds one slot while processing an item;
	// when a worker parks to fan a big zip's members out through the same
	// semaphore it releases its slot first, so the members reuse that freed core
	// and total in-flight extraction work never exceeds the worker count (no 2x
	// oversubscription). nil until Run sets it.
	extractSem chan struct{}

	// cb guards this run's integration callbacks against panics; nil outside
	// Run (nil-safe methods).
	cb *callbackGuard
}

// cbDebug routes an integration debug callback through the run's panic guard:
// a panicking hook becomes a run error instead of killing the worker goroutine.
func (e *Engine) cbDebug(format string, args ...any) {
	dbg := e.Debug
	if dbg == nil {
		return
	}
	e.cb.run("debug", func() { dbg(format, args...) })
}

type workKind int

const (
	kindFile workKind = iota
	kindArchive
	// kindEnvCopy is a loose env/key file discovered under -env: copied to the
	// side directory, never ULP-parsed.
	kindEnvCopy
	// kindTelegramCopy is a loose Telegram tdata folder discovered under -env:
	// copied whole into the side directory, never ULP-parsed.
	kindTelegramCopy
)

// assemblyKind tells processArchive how a multi-part archive item's volumes
// combine into one logical archive.
type assemblyKind int

const (
	assemblySingle     assemblyKind = iota // path is the whole archive (volumes unused)
	assemblyRarVolumes                     // .partN.rar set, read via rardecode.OpenReader
	assemblySplitParts                     // raw byte-split (.zip.NNN/.7z.NNN), read via a concatenated reader
)

type workItem struct {
	path   string
	kind   workKind
	weight int64
	logKey string // identifies the parent "log" unit this item belongs to
	// volumes, when len > 1, holds the ordered on-disk parts of a multi-part
	// set (path is volumes[0]); assembly says how to combine them. Empty for
	// ordinary single-file archives.
	volumes  []string
	assembly assemblyKind
	// missingFirstVolume marks an orphaned continuation part (e.g. a stray
	// .part2.rar with no .part1.rar, or an incomplete .zip.NNN set); it is
	// reported as a skip rather than opened.
	missingFirstVolume bool
	historyCandidate   history.Candidate
}

// accum holds the concurrent-safe counters and result/issue lists shared by the
// worker goroutines. The writer owns emitted/duplicate/credential counts.
type accum struct {
	filesScanned     atomic.Int64
	archivesScanned  atomic.Int64
	skippedFiles     atomic.Int64
	skippedArchives  atomic.Int64
	passwordNotFound atomic.Int64
	parseErrors      atomic.Int64
	openErrors       atomic.Int64
	readErrors       atomic.Int64
	noULP            atomic.Int64
	missingVolumes   atomic.Int64
	ambiguityTotal   atomic.Int64
	ambiguityPath    atomic.Int64
	ambiguityPort    atomic.Int64

	mu      sync.Mutex
	issues  []Issue
	results []SourceResult
	// onIssue mirrors Engine.OnIssue: an uncapped per-issue tee (may be nil).
	onIssue func(path string, kind IssueKind, err error)

	// logRemaining counts unprocessed items per log unit; when a log's last
	// item finishes, the log is counted done. Guarded by logMu.
	logMu        sync.Mutex
	logRemaining map[string]int
}

func (a *accum) addAmbiguityCounts(c AmbiguityCounts) {
	a.ambiguityTotal.Add(int64(c.Total))
	a.ambiguityPath.Add(int64(c.PathOrPassword))
	a.ambiguityPort.Add(int64(c.PortOrLogin))
}
func (a *accum) ambiguityCallback(enabled bool) func(AmbiguityCounts) {
	if !enabled {
		return nil
	}
	return a.addAmbiguityCounts
}

// finishLog decrements the item count for a log unit and reports whether that
// was the unit's final item (so the caller can bump the completed-log count).
func (a *accum) finishLog(key string) bool {
	a.logMu.Lock()
	n := a.logRemaining[key] - 1
	a.logRemaining[key] = n
	a.logMu.Unlock()
	return n == 0
}

func (a *accum) addIssue(path string, kind IssueKind, err error) {
	a.mu.Lock()
	n := 0
	for i := range a.issues {
		if a.issues[i].Kind == kind {
			n++
		}
	}
	if n < perKindIssueCap {
		a.issues = append(a.issues, Issue{Path: path, Kind: kind, Err: err})
	}
	a.mu.Unlock()
	// Tee every issue (not just the capped examples) to the streaming sink,
	// outside the lock so file I/O never serializes the workers on a.mu.
	if a.onIssue != nil {
		a.onIssue(path, kind, err)
	}
}

func (a *accum) addResult(path string, isArchive, ok, hadIssue, historyComplete bool) {
	a.mu.Lock()
	a.results = append(a.results, SourceResult{Path: path, IsArchive: isArchive, OK: ok, HadIssue: hadIssue, HistoryComplete: historyComplete})
	a.mu.Unlock()
}

func (a *accum) snapshotResults() []SourceResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]SourceResult, len(a.results))
	copy(out, a.results)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// HistoryPreparation is the precomputed result of an engine run's discovery
// and history check: the pending work list, the history hits, and the check
// counters. It is produced by PrepareHistory and consumed by RunPrepared, so
// a frontend can render the checking phase on the inline pre-pass bar before
// its live monitor starts. The zero value is not usable; always come from
// PrepareHistory.
type HistoryPreparation struct {
	items   []workItem
	hits    []SourceResult
	checked int
	skipped int
}

// Skipped reports how many sources history marked complete at prepare time
// (frontend-facing: the ingest TUI shows the count in the header badge slot).
func (p HistoryPreparation) Skipped() int { return p.skipped }

// PrepareHistory discovers the input's sources and runs the history
// prehash/lookup — the prefix of Run that reads source files. Nothing here
// writes outputs, so a frontend can run it before its live monitor starts.
func (e *Engine) PrepareHistory(ctx context.Context, input string) (HistoryPreparation, error) {
	envCap := e.EnvMaxLen
	if envCap <= 0 {
		envCap = EnvCopyMaxLen
	}
	envExtra := e.EnvCopier != nil
	items, err := buildWorklist(input, envExtra, envCap, e.Progress)
	if err != nil {
		return HistoryPreparation{}, err
	}
	prep := HistoryPreparation{items: items}
	if e.History != nil {
		prep.items, prep.hits, prep.checked, prep.skipped, err = e.selectHistory(ctx, items)
		if err != nil {
			return HistoryPreparation{}, err
		}
	}
	return prep, nil
}

// Run discovers sources under input, extracts credentials concurrently, and
// writes deduplicated ULP lines to w. It returns aggregate stats and per-source
// results (used by callers to decide -del eligibility).
func (e *Engine) Run(ctx context.Context, input string, w io.Writer) (ExtractStats, []SourceResult, error) {
	prep, err := e.PrepareHistory(ctx, input)
	if err != nil {
		return ExtractStats{}, nil, err
	}
	return e.RunPrepared(ctx, w, prep)
}

// RunPrepared continues a run whose discovery and history check already
// happened through PrepareHistory.
func (e *Engine) RunPrepared(ctx context.Context, w io.Writer, prep HistoryPreparation) (ExtractStats, []SourceResult, error) {
	items, historyHits, historyChecked, historySkipped := prep.items, prep.hits, prep.checked, prep.skipped
	var total int64
	var nFiles, nArchives int
	logRemaining := make(map[string]int, len(items))
	for _, it := range items {
		total += it.weight
		logRemaining[it.logKey]++
		switch it.kind {
		case kindArchive:
			nArchives++
		case kindEnvCopy:
			// Flat env files are not a separate debug bucket.
		case kindTelegramCopy:
			// Loose tdata dirs are paced as unit-weight items, not files.
		default:
			nFiles++
		}
	}
	// runCtx lets the writer abort the workers: if the output write fails, the
	// writer stops draining `lines`, so without cancellation workers would
	// block forever on a full channel. Cancelling unblocks emitAll/feed.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	// Guard every integration callback for the whole run (including the
	// pre-worker discovery debug below): the first panic cancels runCtx and is
	// returned as a run error; later callbacks become no-ops.
	e.cb = newCallbackGuard(runCancel)
	// Engine cancellation (and the deferred runCancel on any return path) must
	// cancel the copier so a full copy queue can never block worker shutdown.
	// The direct defer makes exit-cancellation visible before Run returns; the
	// watcher covers mid-run cancellation while workers are still alive.
	if e.EnvCopier != nil {
		defer e.EnvCopier.cancelRun()
		go func() {
			<-runCtx.Done()
			e.EnvCopier.cancelRun()
		}()
	}

	if e.Debug != nil {
		e.cbDebug("discovered %d source(s): %d file(s), %d archive(s), %d log unit(s), %d byte(s)",
			len(items), nFiles, nArchives, len(logRemaining), total)
	}
	if e.Progress != nil {
		e.Progress.setTotal(total)
		e.Progress.setLogsTotal(int64(len(logRemaining)))
		e.Progress.setPhase(phaseExtract)
	}

	workers := e.Workers
	if workers < 1 {
		workers = 1
	}
	if e.Progress != nil {
		e.Progress.SetWorkers(workers)
	}
	// Shared extraction budget: workers hold a slot per item and lend it to a
	// big zip's member pool while parked, so total in-flight extraction work
	// stays bounded by the worker count. Pre-set only by tests that sample
	// occupancy; production always allocates it here.
	if e.extractSem == nil {
		e.extractSem = make(chan struct{}, workers)
	}

	var acc accum
	acc.logRemaining = logRemaining
	if e.OnIssue != nil {
		acc.onIssue = func(path string, kind IssueKind, err error) {
			e.cb.run("issue", func() { e.OnIssue(path, kind, err) })
		}
	}
	acc.results = append(acc.results, historyHits...)
	jobs := make(chan workItem)
	lines := make(chan string, 4096)

	var writeStats WriteStats
	var writeErr error
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		writeStats, writeErr = runWriter(lines, w, e.Progress, e.DedupKey)
		if writeErr != nil {
			runCancel()
		}
	}()

	var workerWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for it := range jobs {
				if runCtx.Err() != nil {
					continue
				}
				// Hold one extraction slot for this item; a parallel zip lends
				// it to its member pool (see readZipFiles) so the budget is
				// global, not additive. The matching live-status slot is leased
				// per item (not pinned to this goroutine) so dispatched members
				// and password probes get their own rows in the panel.
				e.extractSem <- struct{}{}
				slot := e.Progress.acquireSlot()
				e.process(runCtx, slot, it, lines, &acc)
				e.Progress.releaseSlot(slot)
				<-e.extractSem
			}
		}()
	}

feed:
	for _, it := range items {
		select {
		case <-runCtx.Done():
			break feed
		case jobs <- it:
		}
	}
	close(jobs)
	workerWG.Wait()
	close(lines)
	writerWG.Wait()

	if e.Progress != nil && !e.FollowedByIngest {
		e.Progress.setPhase(phaseDone)
	}

	stats := ExtractStats{
		FilesScanned:            int(acc.filesScanned.Load()),
		ArchivesScanned:         int(acc.archivesScanned.Load()),
		Logs:                    len(logRemaining),
		Credentials:             writeStats.Seen,
		AmbiguityTotal:          int(acc.ambiguityTotal.Load()),
		AmbiguityPathOrPassword: int(acc.ambiguityPath.Load()),
		AmbiguityPortOrLogin:    int(acc.ambiguityPort.Load()),
		Emitted:                 writeStats.Emitted,
		Duplicates:              writeStats.Duplicates,
		SkippedFiles:            int(acc.skippedFiles.Load()),
		SkippedArchives:         int(acc.skippedArchives.Load()),
		PasswordNotFound:        int(acc.passwordNotFound.Load()),
		ParseErrors:             int(acc.parseErrors.Load()),
		OpenErrors:              int(acc.openErrors.Load()),
		ReadErrors:              int(acc.readErrors.Load()),
		NoULP:                   int(acc.noULP.Load()),
		MissingVolumes:          int(acc.missingVolumes.Load()),
		HistoryChecked:          historyChecked,
		HistorySkipped:          historySkipped,
		Issues:                  acc.issues,
	}
	results := acc.snapshotResults()
	candidates := make(map[string]history.Candidate, len(items))
	for _, it := range items {
		if len(it.historyCandidate.Paths) > 0 {
			candidates[it.path] = it.historyCandidate
		}
	}
	for i := range results {
		if c, ok := candidates[results[i].Path]; ok {
			results[i].HistoryCandidate = c
		}
	}
	if writeErr != nil {
		return stats, results, writeErr
	}
	// A panicked integration callback is a run error (never an archive parse
	// issue): the run stops and no source may be treated as -del eligible.
	if cerr := e.cb.callbackErr(); cerr != nil {
		return stats, results, cerr
	}
	if e.EnvCopier != nil {
		if cerr := e.EnvCopier.CallbackError(); cerr != nil {
			return stats, results, cerr
		}
	}
	if ctx.Err() != nil {
		return stats, results, ctx.Err()
	}
	return stats, results, nil
}

func (e *Engine) selectHistory(ctx context.Context, items []workItem) ([]workItem, []SourceResult, int, int, error) {
	var historyTotal int64
	for _, it := range items {
		if (it.kind == kindFile || it.kind == kindArchive) && !it.missingFirstVolume {
			historyTotal += it.weight
		}
	}
	e.Progress.BeginHistory(historyTotal)
	eligible := make([]int, 0, len(items))
	units := make([]history.FingerprintUnit, 0, len(items))
	for i := range items {
		it := &items[i]
		if it.kind != kindFile && it.kind != kindArchive {
			continue
		}
		// Incomplete sets (missingFirstVolume: an orphan part with no set
		// head, or a gapped set's head) fingerprint the parts that exist, so
		// the first run reports the issue and records the source and later
		// -history runs skip it instead of re-reporting a phantom archive
		// every time. Lockout-safe: the identity covers only the parts on
		// disk, so completing the set changes the fingerprint and the full
		// set is processed normally.
		if len(it.volumes) > 1 {
			label := "rar-volumes"
			if it.assembly == assemblySplitParts {
				label = "split-parts"
			}
			// A multipart assembly is hashed internally sequentially (part
			// order defines the identity), but whole assemblies are
			// distributed across workers like single files.
			units = append(units, history.FingerprintUnit{Label: label, Volumes: it.volumes})
		} else {
			units = append(units, history.FingerprintUnit{Path: it.path})
		}
		eligible = append(eligible, i)
	}
	// FingerprintAll returns candidates indexed by input (eligible) order
	// whatever the completion order, so candidate order — and therefore hit
	// selection and pending order — is identical to the sequential pass. The
	// first failing unit cancels the rest; its error surfaces here.
	var lastReported atomic.Int64
	progress := func(_, _ int, bytesDone, _ int64) {
		e.Progress.addHistoryBytes(bytesDone - lastReported.Swap(bytesDone))
	}
	candidates, err := history.FingerprintAll(ctx, units, 0, progress)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	for j, c := range candidates {
		items[eligible[j]].historyCandidate = c
	}
	selection, err := history.Select(ctx, e.History, candidates)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	hitIDs := make(map[history.Identity]struct{}, len(selection.Hits))
	for _, c := range selection.Hits {
		hitIDs[c.ID] = struct{}{}
	}
	pending := make([]workItem, 0, len(items)-len(selection.Hits))
	hits := make([]SourceResult, 0, len(selection.Hits))
	for i, it := range items {
		if _, ok := hitIDs[it.historyCandidate.ID]; ok && len(it.historyCandidate.Paths) > 0 {
			hits = append(hits, SourceResult{Path: it.path, IsArchive: it.kind == kindArchive,
				OK: true, HistoryComplete: true, HistoryCandidate: it.historyCandidate, HistoryHit: true})
			continue
		}
		pending = append(pending, items[i])
	}
	return pending, hits, len(eligible), len(hits), nil
}

func (e *Engine) process(ctx context.Context, idx int, it workItem, lines chan<- string, acc *accum) {
	if e.Progress != nil {
		e.Progress.setCurrent(it.path)
		e.Progress.setActive(idx, it.path, StageOpening)
		// The slot is cleared and returned to the free-list by releaseSlot in
		// the worker loop once this item finishes, so no clearActive here.
	}
	switch it.kind {
	case kindArchive:
		e.processArchive(ctx, idx, it, lines, acc)
	case kindEnvCopy:
		e.processEnvFile(ctx, idx, it, acc)
	case kindTelegramCopy:
		e.processTelegramDir(ctx, idx, it, acc)
	default:
		e.processFile(ctx, idx, it, lines, acc)
	}
	if acc.finishLog(it.logKey) && e.Progress != nil {
		e.Progress.addLogDone()
	}
}

func (e *Engine) processFile(ctx context.Context, idx int, it workItem, lines chan<- string, acc *accum) {
	acc.filesScanned.Add(1)
	if e.Progress != nil {
		e.Progress.addFile()
		e.Progress.setStage(idx, StageParsing)
	}
	cr := newCreditor(e.Progress, it.weight, 1)
	defer cr.finish()

	f, err := os.Open(it.path)
	if err != nil {
		acc.skippedFiles.Add(1)
		acc.openErrors.Add(1)
		acc.addIssue(it.path, IssueOpenError, err)
		// preservation disabled 2026-09-30 (user): this failure no longer withholds
		// history — when a source is done, it is done.
		// acc.addResult(it.path, false, false, false, false)
		acc.addResult(it.path, false, false, false, true)
		if e.Debug != nil {
			e.cbDebug("file %s: open error: %v", it.path, err)
		}
		return
	}
	unreg := registerAbort(ctx, f)
	// Parse streams into a bounded sink and only replays to the writer after
	// the whole file parsed cleanly, so a parse failure mid-file emits nothing
	// (the same all-or-nothing contract the slice parse had) while a dense
	// file costs a bounded memory head plus a temp-file spill.
	sink := newCredSink(e.TempDir, it.path)
	mixed, perr := ParseCredentialsStreamWithDiagnostics(countingReader{r: f, c: cr}, it.path, e.TempDir, e.Loose, e.Debug != nil, acc.ambiguityCallback(e.Debug != nil), sink.add)
	closeErr := f.Close()
	unreg()
	if perr != nil || closeErr != nil {
		sink.discard()
		acc.skippedFiles.Add(1)
		acc.parseErrors.Add(1)
		acc.addIssue(it.path, IssueParseError, firstErr(perr, closeErr))
		// preservation disabled 2026-09-30 (user): this failure no longer withholds
		// history — when a source is done, it is done.
		// acc.addResult(it.path, false, false, false, false)
		acc.addResult(it.path, false, false, false, true)
		if e.Debug != nil {
			e.cbDebug("file %s: parse error: %v", it.path, firstErr(perr, closeErr))
		}
		return
	}
	emitted, gateRejected := 0, 0
	formatter := ulpengine.NewStableFormatter()
	if err := sink.replay(func(c Credential) error {
		line, err := e.formatCredentialWith(formatter, c)
		if err != nil {
			gateRejected++
			acc.parseErrors.Add(1)
			path := c.Source
			if path == "" {
				path = it.path
			}
			acc.addIssue(path, IssueParseError, err)
			if e.Debug != nil {
				e.cbDebug("file %s: emitted credential rejected by shared parser: %v", path, err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case lines <- line:
			emitted++
			return nil
		}
	}); err != nil {
		sink.discard()
		acc.skippedFiles.Add(1)
		acc.parseErrors.Add(1)
		acc.addIssue(it.path, IssueParseError, err)
		// preservation disabled 2026-09-30 (user): this failure no longer withholds
		// history — when a source is done, it is done.
		// acc.addResult(it.path, false, false, false, false)
		acc.addResult(it.path, false, false, false, true)
		if e.Debug != nil {
			e.cbDebug("file %s: parse error: %v", it.path, err)
		}
		return
	}
	sink.discard()
	hadIssue := mixed || gateRejected > 0
	if mixed {
		// Product decision (review H-20): labeled precedence is kept and the
		// output is unchanged, but the discarded colon lines are valid
		// credentials. Flag the source as an issue; since 2026-09-30 this is a
		// parse-quality outcome and no longer withholds history or blocks -del.
		acc.addIssue(it.path, IssueMixedFormat, nil)
		if e.Debug != nil {
			e.cbDebug("file %s: mixed labeled and colon formats; colon results discarded", it.path)
		}
	}
	if emitted == 0 {
		if gateRejected == 0 {
			acc.noULP.Add(1)
			acc.addIssue(it.path, IssueNoULP, nil)
			if e.Debug != nil {
				e.cbDebug("file %s: no ULP detected", it.path)
			}
		}
		acc.addResult(it.path, false, true, true, true)
		return
	}
	// Only extraction failures reached above withhold history; parse-quality
	// outcomes surface as issues but the source still records.
	acc.addResult(it.path, false, true, hadIssue, true)
	if e.Debug != nil {
		e.cbDebug("file %s: %d credentials", it.path, emitted)
	}
}

// processEnvFile copies a loose env/key file under -env. Best-effort throughout.
func (e *Engine) processEnvFile(ctx context.Context, idx int, it workItem, acc *accum) {
	if e.EnvCopier == nil {
		return
	}
	cr := newCreditor(e.Progress, it.weight, 1)
	defer cr.finish()

	e.EnvCopier.EnqueueFile(it.path)
	// preservation disabled 2026-09-30 (user): env sources record like any other.
	// acc.addResult(it.path, false, true, false, false)
	acc.addResult(it.path, false, true, false, true)
	if e.Debug != nil {
		e.cbDebug("env file %s: queued for copy", it.path)
	}
}

// processTelegramDir copies a loose Telegram tdata folder under -env.
// The copy is synchronous so OK reflects success and -del keeps the source
// on failure. Never ULP-parsed.
func (e *Engine) processTelegramDir(ctx context.Context, idx int, it workItem, acc *accum) {
	if e.EnvCopier == nil {
		return
	}
	cr := newCreditor(e.Progress, it.weight, 1)
	defer cr.finish()

	if ctx.Err() != nil {
		// preservation disabled 2026-09-30 (user).
		// acc.addResult(it.path, false, false, true, false)
		acc.addResult(it.path, false, false, true, true)
		return
	}
	err := e.EnvCopier.CopyDir(it.path)
	// preservation disabled 2026-09-30 (user).
	// acc.addResult(it.path, false, err == nil, err != nil, false)
	acc.addResult(it.path, false, err == nil, err != nil, true)
	if e.Debug != nil {
		if err != nil {
			e.cbDebug("telegram tdata %s: copy error: %v", it.path, err)
		} else {
			e.cbDebug("telegram tdata %s: copied", it.path)
		}
	}
}

func (e *Engine) processArchive(ctx context.Context, idx int, it workItem, lines chan<- string, acc *accum) {
	acc.archivesScanned.Add(1)
	if e.Progress != nil {
		e.Progress.addArchive()
	}
	if it.missingFirstVolume {
		// Orphaned multi-volume continuation part: report the gap (so the user
		// sees it) and credit its bytes so the progress bar still completes.
		newCreditor(e.Progress, it.weight, 1).finish()
		acc.skippedArchives.Add(1)
		acc.missingVolumes.Add(1)
		acc.addIssue(it.path, IssueMissingVolume, errMissingFirstVolume)
		// preservation disabled 2026-09-30 (user): orphaned volume parts no longer
		// withhold history.
		// acc.addResult(it.path, true, false, true, false)
		acc.addResult(it.path, true, false, true, true)
		if e.Debug != nil {
			e.cbDebug("archive %s: %v; skipped", it.path, errMissingFirstVolume)
		}
		return
	}
	// onIssue records a per-member problem without aborting the parent archive.
	// Called from this worker and parallel ZIP tasks, so the source flags are
	// atomic. hadIssue drives exit classification/summary; hadExtractIssue now
	// only feeds the NoULP double-report guard — since 2026-09-30 (user
	// decision) history completeness no longer depends on it. Mixed-format
	// members are parse-quality (deterministic discards): reported but not
	// completeness-blocking.
	var hadIssue, hadExtractIssue atomic.Bool
	onIssue := func(path string, kind IssueKind, err error) {
		hadIssue.Store(true)
		if kind != IssueMixedFormat {
			hadExtractIssue.Store(true)
		}
		switch kind {
		case IssuePasswordNotFound:
			acc.passwordNotFound.Add(1)
		case IssueOpenError:
			acc.openErrors.Add(1)
		case IssueReadError:
			acc.readErrors.Add(1)
		case IssueMissingVolume:
			acc.missingVolumes.Add(1)
		case IssueEnvCopy:
			// EnvWriteErrors is counted on the copier; this only blocks -del.
		default:
			acc.parseErrors.Add(1)
		}
		acc.addIssue(path, kind, err)
		if e.Debug != nil {
			e.cbDebug("archive member %s: %s: %v", path, kind, err)
		}
	}

	// Validated archive readers stream candidates directly, but every candidate
	// still crosses the shared-parser emit gate before output/library ingest.
	// Commit loops serialize emit, so emitted and gateRejected need no atomic
	// synchronization.
	//
	// Gate rejects are parse-quality outcomes, not extraction failures: they
	// are reported as issues but do NOT set hadIssue, so the source still
	// records in history (product decision 2026-09-30 — rejected lines are
	// deterministic, and history + -del are keyed on extraction success).
	var emitted, gateRejected int
	formatter := ulpengine.NewStableFormatter()
	emit := func(c Credential) {
		line, err := e.formatCredentialWith(formatter, c)
		if err != nil {
			path := c.Source
			if path == "" {
				path = it.path
			}
			gateRejected++
			// Reported as an issue (exit classification) but NOT an extraction
			// failure: the source still records in history.
			hadIssue.Store(true)
			acc.parseErrors.Add(1)
			acc.addIssue(path, IssueParseError, err)
			if e.Debug != nil {
				e.cbDebug("archive member %s: %s: %v", path, IssueParseError, err)
			}
			return
		}
		select {
		case <-ctx.Done():
		case lines <- line:
			emitted++
		}
	}
	ec := extractCtx{
		passwords: e.Passwords,
		winner:    &e.winner,
		tempDir:   e.TempDir,
		display:   it.path,
		emit:      emit,
		onIssue:   onIssue,
		p:         e.Progress,
		setStage:  func(s WorkerStage) { e.Progress.setStage(idx, s) },
		setItem:   func(label string) { e.Progress.setWorkerPath(idx, label) },
		debug:     e.cbDebug,
		sem:       e.extractSem,
		processor: credentialParser{loose: e.Loose, diagnostics: e.Debug != nil, onAmbiguity: acc.ambiguityCallback(e.Debug != nil)},
		env:       e.EnvCopier,
		spill:     newSpillBudget(0, 0),
	}
	// One heartbeat throttle per top-level item, shared across the whole
	// recursion. Set here (not just in readArchiveCredentials) so the
	// multi-volume and split paths -- which dispatch directly below and are the
	// longest-running archives -- still emit "still extracting" lines.
	if e.Debug != nil {
		ec.hb = newDebugThrottle(5 * time.Second)
	}
	scan, err := func() (scan archiveScan, err error) {
		defer recoverAsError(&err, ec)
		switch it.assembly {
		case assemblyRarVolumes:
			return readRarVolumes(ctx, it.volumes, ec, it.weight)
		case assemblySplitParts:
			return readSplitArchive(ctx, it.volumes, ec, it.weight)
		default:
			return readArchiveCredentials(ctx, it.path, ec, it.weight)
		}
	}()
	acc.filesScanned.Add(int64(scan.files))
	acc.archivesScanned.Add(int64(scan.nestedArchives)) // top-level archive already counted above
	if e.Progress != nil {
		e.Progress.addArchives(int64(scan.nestedArchives)) // keep live count == summary
	}
	if err != nil {
		acc.skippedArchives.Add(1)
		if errors.Is(err, errPasswordNotFound) {
			acc.passwordNotFound.Add(1)
			acc.addIssue(it.path, IssuePasswordNotFound, err)
			if e.Debug != nil {
				e.cbDebug("archive %s: password not found", it.path)
			}
		} else {
			acc.parseErrors.Add(1)
			acc.addIssue(it.path, IssueParseError, err)
			if e.Debug != nil {
				e.cbDebug("archive %s: parse error: %v", it.path, err)
			}
		}
		// preservation disabled 2026-09-30 (user): archive scan failures no longer
		// withhold history.
		// acc.addResult(it.path, true, false, hadIssue.Load(), false)
		acc.addResult(it.path, true, false, hadIssue.Load(), true)
		return
	}
	// preservation disabled 2026-09-30 (user): extraction failures (locked
	// members, missing volumes, corrupt data) no longer withhold history —
	// when an archive is done, anything inside it is considered done.
	// historyComplete := !hadExtractIssue.Load()
	historyComplete := true
	if emitted == 0 && gateRejected == 0 && !hadExtractIssue.Load() {
		// The archive streamed successfully and produced no credentials: a
		// genuine "no credential files found". An archive whose credential
		// members were found but failed (isolated parse/open issues already
		// recorded above) must NOT also claim no credentials were found —
		// that second message is factually false and doubles the report.
		acc.noULP.Add(1)
		acc.addIssue(it.path, IssueNoULP, nil)
		hadIssue.Store(true)
		if e.Debug != nil {
			e.cbDebug("archive %s: no ULP detected", it.path)
		}
	}
	acc.addResult(it.path, true, true, hadIssue.Load(), historyComplete)
	if e.Debug != nil && emitted > 0 {
		e.cbDebug("archive %s: %d credentials across %d file(s), %d nested archive(s)",
			it.path, emitted, scan.files, scan.nestedArchives)
	}
}

func (e *Engine) formatCredentialWith(formatter *ulpengine.StableFormatter, c Credential) (string, error) {
	rawURL := credentialURL(c, e.NoURI)
	host, url, login, password, ok := ulpengine.ValidateFields(rawURL, c.Username, c.Password, e.Loose)
	if !ok {
		if len(strings.TrimSpace(c.Password)) > 64 {
			return "", fmt.Errorf("shared parser rejected credential: password>64")
		}
		return "", fmt.Errorf("shared parser rejected credential: malformed")
	}
	line, ok := formatter.FormatRecordStable(host, url, login, password, e.NoURI)
	// Lane gate: strict-lane admissions already satisfy the raw consumer's
	// strict rules by construction (the admission used ValidateFields' strict
	// predicate, shared with StrictLaneAdmitted), so re-running the raw
	// parser here only burns wall time. Loose-only-branch and android
	// admissions keep the full dual-fidelity check below.
	strictLane := ulpengine.StrictLaneAdmitted(rawURL, c.Username, c.Password)
	if ok && strictLane {
		return line, nil
	}
	if ok {
		// Dual-fidelity emission: the stored-stable line must also decode,
		// through the raw consumer used by DedupKeyForLine and downstream
		// ingest, to exactly the supplied tuple. This is a fidelity check on
		// generated output — never a way to select or mutate the known fields.
		if h, _, l, p, ok := ulpengine.ParseLine(line, e.Loose); ok &&
			h == host && l == login && p == password {
			return line, nil
		}
	}
	// One explicit host-only attempt, same expected tuple. FormatRecordStable
	// re-verifies the stored round trip; the raw check must agree too. This
	// fallback cannot rescue a credential the field validator already
	// rejected above.
	if !e.NoURI && !strictLane {
		if line, ok := formatter.FormatRecordStable(host, host, login, password, true); ok {
			if h, _, l, p, ok := ulpengine.ParseLine(line, e.Loose); ok &&
				h == host && l == login && p == password {
				return line, nil
			}
		}
	}
	return "", fmt.Errorf("shared parser rejected credential: malformed")
}

// runWriter is the single fan-in consumer. It deduplicates by a uint64 key so
// memory stays at ~8 bytes per unique line rather than the full string. keyOf
// (when non-nil) yields the library's canonical host:login:password key so the
// unique set matches what the library stores; lines it can't key (nil keyer, or
// a line the library would reject) fall back to the whole-line hash so distinct
// lines never merge.
func runWriter(lines <-chan string, w io.Writer, p *Progress, keyOf func(string) (uint64, bool)) (WriteStats, error) {
	bw := bufio.NewWriter(w)
	seen := make(map[uint64]struct{}, 1<<14)
	var stats WriteStats
	for line := range lines {
		stats.Seen++
		h, ok := uint64(0), false
		if keyOf != nil {
			h, ok = keyOf(line)
		}
		if !ok {
			h = xxhash.Sum64String(line)
		}
		if _, ok := seen[h]; ok {
			stats.Duplicates++
			p.addDup()
			continue
		}
		seen[h] = struct{}{}
		if _, err := bw.WriteString(line); err != nil {
			return stats, err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return stats, err
		}
		stats.Emitted++
		p.addEmitted()
	}
	if err := bw.Flush(); err != nil {
		return stats, err
	}
	return stats, nil
}

// buildWorklist scans input once, assigning each source its on-disk weight and
// log-group key. Progress is credited per discovered source so the SCANNING
// phase shows live motion instead of a frozen 0%.
// envExtra enqueues kindEnvCopy. envCap bounds progress weight for capped
// reads. It is inert when off.
func buildWorklist(input string, envExtra bool, envCap int64, prog *Progress) ([]workItem, error) {
	absRoot, rootIsDir, err := rootMeta(input)
	if err != nil {
		return nil, err
	}
	var filesP, archivesP, envP, tgP []string
	err = walkSources(input, envExtra, func(path string, kind sourceKind) {
		switch kind {
		case sourceArchive:
			archivesP = append(archivesP, path)
		case sourcePassword:
			filesP = append(filesP, path)
		case sourceEnv:
			envP = append(envP, path)
		case sourceTelegram:
			tgP = append(tgP, path)
		}
		prog.addDiscovered()
	})
	if err != nil {
		return nil, err
	}
	// A single-file input names only one part of a multi-volume / split set; the
	// walk never sees its siblings, so weight (bar pacing) and the "part N/M"
	// label would be wrong even though rardecode / the split reader follow the
	// chain on disk regardless. Expand to the full on-disk set so accounting is
	// correct. One ReadDir for the one named file -- no RDP fan-out, and the
	// directory-walk path (which already enqueues every part) is untouched.
	if !rootIsDir && len(archivesP) == 1 {
		archivesP = VolumeSet(archivesP[0])
	}
	sort.Strings(filesP)
	sort.Strings(archivesP)
	sort.Strings(envP)
	sort.Strings(tgP)

	items := make([]workItem, 0, len(filesP)+len(archivesP)+len(envP)+len(tgP))
	for _, f := range filesP {
		items = append(items, workItem{path: f, kind: kindFile, weight: fileWeight(f), logKey: logGroupKey(absRoot, rootIsDir, f)})
	}
	for _, f := range envP {
		items = append(items, workItem{path: f, kind: kindEnvCopy, weight: cappedWeight(f, envCap), logKey: logGroupKey(absRoot, rootIsDir, f)})
	}
	for _, f := range tgP {
		items = append(items, workItem{path: f, kind: kindTelegramCopy, weight: telegramDirWeight(f), logKey: logGroupKey(absRoot, rootIsDir, f)})
	}
	keyOf := func(a string) string { return logGroupKey(absRoot, rootIsDir, a) }
	items = append(items, groupArchiveVolumes(archivesP, fileWeight, keyOf)...)
	return items, nil
}

// cappedWeight is fileWeight bounded by cap (bar pacing only): an env file
// is read only up to the cap, so a multi-GB file shouldn't inflate
// the progress total by its full on-disk size.
func cappedWeight(path string, cap int64) int64 {
	w := fileWeight(path)
	if cap > 0 && w > cap {
		return cap
	}
	return w
}

func rootMeta(input string) (absRoot string, isDir bool, err error) {
	info, err := os.Stat(input)
	if err != nil {
		return "", false, err
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", false, err
	}
	return filepath.Clean(abs), info.IsDir(), nil
}

// registerAbort tracks f with the context's fileabort registry (if any) so a
// graceful Ctrl-C can close it and unstick a blocked read. The returned func
// unregisters once the read finishes normally.
func registerAbort(ctx context.Context, f *os.File) func() {
	if reg := fileabort.FromContext(ctx); reg != nil {
		return reg.Register(f)
	}
	return func() {}
}

func fileWeight(path string) int64 {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return fi.Size()
	}
	return 1
}

// telegramDirWeight paces a loose tdata folder as one logical item. CopyDir
// runs synchronously on the engine worker; walking a large tree twice (weight
// then copy) would double I/O for no pacing benefit.
func telegramDirWeight(path string) int64 {
	return 1
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
