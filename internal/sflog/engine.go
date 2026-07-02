package sflog

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

// errPasswordNotFound marks an archive that no password candidate could open.
var errPasswordNotFound = errors.New("password not found")

// errMissingFirstVolume marks an orphaned multi-volume RAR continuation part
// (e.g. a stray name.part2.rar with no name.part1.rar present).
var errMissingFirstVolume = errors.New("first volume (.part1.rar) not found")

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
	Passwords []string
	Progress  *Progress
	Debug     func(format string, args ...any)
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
	// SecretSink (optional) receives non-credential member bytes for secret
	// scanning. nil disables secret scanning entirely (the credential path is
	// then byte-for-byte unchanged). SecretMaxLen caps per-member bytes read
	// (0 -> defaultSecretMaxLen).
	SecretSink   SecretSink
	SecretMaxLen int64
	// FollowedByIngest tells Run to leave the tracker in the extract phase
	// instead of flipping to Done, so an -od caller can hand straight off to the
	// ingest phase without a transient "COMPLETE" frame.
	FollowedByIngest bool

	// extractSem is the engine-wide extraction budget (cap = worker count),
	// allocated in Run. Every worker holds one slot while processing an item;
	// when a worker parks to fan a big zip's members out through the same
	// semaphore it releases its slot first, so the members reuse that freed core
	// and total in-flight extraction work never exceeds the worker count (no 2x
	// oversubscription). nil until Run sets it.
	extractSem chan struct{}
}

type workKind int

const (
	kindFile workKind = iota
	kindArchive
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
	noULP            atomic.Int64
	missingVolumes   atomic.Int64

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

func (a *accum) addResult(path string, isArchive, ok, hadIssue bool) {
	a.mu.Lock()
	a.results = append(a.results, SourceResult{Path: path, IsArchive: isArchive, OK: ok, HadIssue: hadIssue})
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

// Run discovers sources under input, extracts credentials concurrently, and
// writes deduplicated ULP lines to w. It returns aggregate stats and per-source
// results (used by callers to decide -del eligibility).
func (e *Engine) Run(ctx context.Context, input string, w io.Writer) (ExtractStats, []SourceResult, error) {
	items, err := buildWorklist(input, e.Progress)
	if err != nil {
		return ExtractStats{}, nil, err
	}
	var total int64
	var nFiles, nArchives int
	logRemaining := make(map[string]int, len(items))
	for _, it := range items {
		total += it.weight
		logRemaining[it.logKey]++
		if it.kind == kindArchive {
			nArchives++
		} else {
			nFiles++
		}
	}
	if e.Debug != nil {
		e.Debug("discovered %d source(s): %d file(s), %d archive(s), %d log unit(s), %d byte(s)",
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

	// runCtx lets the writer abort the workers: if the output write fails, the
	// writer stops draining `lines`, so without cancellation workers would
	// block forever on a full channel. Cancelling unblocks emitAll/feed.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	var acc accum
	acc.logRemaining = logRemaining
	acc.onIssue = e.OnIssue
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
		FilesScanned:     int(acc.filesScanned.Load()),
		ArchivesScanned:  int(acc.archivesScanned.Load()),
		Logs:             len(logRemaining),
		Credentials:      writeStats.Seen,
		Emitted:          writeStats.Emitted,
		Duplicates:       writeStats.Duplicates,
		SkippedFiles:     int(acc.skippedFiles.Load()),
		SkippedArchives:  int(acc.skippedArchives.Load()),
		PasswordNotFound: int(acc.passwordNotFound.Load()),
		ParseErrors:      int(acc.parseErrors.Load()),
		OpenErrors:       int(acc.openErrors.Load()),
		NoULP:            int(acc.noULP.Load()),
		MissingVolumes:   int(acc.missingVolumes.Load()),
		Issues:           acc.issues,
	}
	results := acc.snapshotResults()
	if writeErr != nil {
		return stats, results, writeErr
	}
	if ctx.Err() != nil {
		return stats, results, ctx.Err()
	}
	return stats, results, nil
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
		acc.addResult(it.path, false, false, false)
		if e.Debug != nil {
			e.Debug("file %s: open error: %v", it.path, err)
		}
		return
	}
	unreg := registerAbort(ctx, f)
	creds, perr := ParseCredentials(countingReader{r: f, c: cr}, it.path)
	closeErr := f.Close()
	unreg()
	if perr != nil || closeErr != nil {
		acc.skippedFiles.Add(1)
		acc.parseErrors.Add(1)
		acc.addIssue(it.path, IssueParseError, firstErr(perr, closeErr))
		acc.addResult(it.path, false, false, false)
		if e.Debug != nil {
			e.Debug("file %s: parse error: %v", it.path, firstErr(perr, closeErr))
		}
		return
	}
	e.emitAll(ctx, lines, creds)
	// Loose files are the direct input; scan them for secrets too (a .env or
	// config passed straight to sfl). The credential read above consumed the
	// stream, so re-open. Best-effort: a re-open failure never fails the file.
	if e.SecretSink != nil {
		if sf, oerr := os.Open(it.path); oerr == nil {
			ec := extractCtx{secrets: e.SecretSink, secretMaxLen: e.SecretMaxLen}
			ec.scanSecrets(ctx, sf, it.path)
			sf.Close()
		}
	}
	if len(creds) == 0 {
		acc.noULP.Add(1)
		acc.addIssue(it.path, IssueNoULP, nil)
		if e.Debug != nil {
			e.Debug("file %s: no ULP detected", it.path)
		}
	}
	acc.addResult(it.path, false, true, false)
	if e.Debug != nil && len(creds) > 0 {
		e.Debug("file %s: %d credentials", it.path, len(creds))
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
		acc.addResult(it.path, true, false, true)
		if e.Debug != nil {
			e.Debug("archive %s: %v; skipped", it.path, errMissingFirstVolume)
		}
		return
	}
	// Stream credentials straight to the writer instead of buffering the whole
	// archive tree: zip resolves its password up front and rar/7z buffer-and-
	// commit internally, so by the time emit fires the data is already
	// validated. emit runs only on this worker goroutine (sequential loops, the
	// rar/7z commit loop, and the parallel chunk merge), so emitted is a plain
	// int. This caps per-archive memory instead of holding every credential.
	var emitted int
	emit := func(c Credential) {
		select {
		case <-ctx.Done():
		case lines <- FormatULPLine(c, e.NoURI):
			emitted++
		}
	}
	// onIssue records a per-member problem (e.g. a nested archive whose password
	// was not found) without aborting the parent archive or the run. hadIssue
	// then keeps the source out of -del so un-extracted data is never discarded.
	// Called only from this worker goroutine, so the plain bool is safe.
	var hadIssue bool
	onIssue := func(path string, kind IssueKind, err error) {
		hadIssue = true
		switch kind {
		case IssuePasswordNotFound:
			acc.passwordNotFound.Add(1)
		case IssueOpenError:
			acc.openErrors.Add(1)
		case IssueMissingVolume:
			acc.missingVolumes.Add(1)
		default:
			acc.parseErrors.Add(1)
		}
		acc.addIssue(path, kind, err)
		if e.Debug != nil {
			e.Debug("archive member %s: %s: %v", path, kind, err)
		}
	}
	ec := extractCtx{
		passwords: e.Passwords,
		tempDir:   e.TempDir,
		display:   it.path,
		emit:      emit,
		onIssue:   onIssue,
		p:         e.Progress,
		setStage:  func(s WorkerStage) { e.Progress.setStage(idx, s) },
		setItem:   func(label string) { e.Progress.setWorkerPath(idx, label) },
		debug:        e.Debug,
		sem:          e.extractSem,
		processor:    defaultProcessor,
		secrets:      e.SecretSink,
		secretMaxLen: e.SecretMaxLen,
	}
	// One heartbeat throttle per top-level item, shared across the whole
	// recursion. Set here (not just in readArchiveCredentials) so the
	// multi-volume and split paths -- which dispatch directly below and are the
	// longest-running archives -- still emit "still extracting" lines.
	if e.Debug != nil {
		ec.hb = newDebugThrottle(5 * time.Second)
	}
	var scan archiveScan
	var err error
	switch it.assembly {
	case assemblyRarVolumes:
		scan, err = readRarVolumes(ctx, it.volumes, ec, it.weight)
	case assemblySplitParts:
		scan, err = readSplitArchive(ctx, it.volumes, ec, it.weight)
	default:
		scan, err = readArchiveCredentials(ctx, it.path, ec, it.weight)
	}
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
				e.Debug("archive %s: password not found", it.path)
			}
		} else {
			acc.parseErrors.Add(1)
			acc.addIssue(it.path, IssueParseError, err)
			if e.Debug != nil {
				e.Debug("archive %s: parse error: %v", it.path, err)
			}
		}
		acc.addResult(it.path, true, false, hadIssue)
		return
	}
	if emitted == 0 {
		acc.noULP.Add(1)
		acc.addIssue(it.path, IssueNoULP, nil)
		if e.Debug != nil {
			e.Debug("archive %s: no ULP detected", it.path)
		}
	}
	acc.addResult(it.path, true, true, hadIssue)
	if e.Debug != nil && emitted > 0 {
		e.Debug("archive %s: %d credentials across %d file(s), %d nested archive(s)",
			it.path, emitted, scan.files, scan.nestedArchives)
	}
}

func (e *Engine) emitAll(ctx context.Context, lines chan<- string, creds []Credential) {
	for _, c := range creds {
		select {
		case <-ctx.Done():
			return
		case lines <- FormatULPLine(c, e.NoURI):
		}
	}
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
func buildWorklist(input string, prog *Progress) ([]workItem, error) {
	absRoot, rootIsDir, err := rootMeta(input)
	if err != nil {
		return nil, err
	}
	var filesP, archivesP []string
	err = walkSources(input, func(path string, isArchive bool) {
		if isArchive {
			archivesP = append(archivesP, path)
		} else {
			filesP = append(filesP, path)
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

	items := make([]workItem, 0, len(filesP)+len(archivesP))
	for _, f := range filesP {
		items = append(items, workItem{path: f, kind: kindFile, weight: fileWeight(f), logKey: logGroupKey(absRoot, rootIsDir, f)})
	}
	keyOf := func(a string) string { return logGroupKey(absRoot, rootIsDir, a) }
	items = append(items, groupArchiveVolumes(archivesP, fileWeight, keyOf)...)
	return items, nil
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

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
