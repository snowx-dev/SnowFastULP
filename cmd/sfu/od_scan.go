package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/searchidx"

	"github.com/klauspost/compress/zstd"
)

// phase 0 (-od): discover prior sfu_*.txt.zst archives at dest, regen
// stale/missing .idx sidecars, route every dest hash into
// <tempDir>/dest_keys/bucket_NNNN.bin. phase 2 pre-loads only its bucket
// (~10 MB at 5B keys, B=4096) so RAM stays bounded regardless of library size.

// 0 = idle, the 2nd TUI frame renders iff phase != idle && phase != done
type odPhase int32

const (
	odPhaseIdle     odPhase = 0
	odPhaseDiscover odPhase = 1
	odPhaseRegen    odPhase = 2
	odPhaseLoad     odPhase = 3
	odPhaseDone     odPhase = 4
	// post-dedup index write for THIS run's output, shares odMetrics
	// shape w/ regen so it reuses the same renderer (just swaps labels)
	odPhaseIndexOwn odPhase = 5
	// narrow window where keys are routed but dest writers are still
	// flushing/closing. without this the load bar sits at 100% during
	// disk I/O and reads as stuck
	odPhaseCommitBuckets odPhase = 6
)

// atomic counters for the TUI's second frame + end-of-run summary.
// lock-free so TUI doesnt coordinate w/ phase-0 workers
type odMetrics struct {
	phase         atomic.Int32
	startedAtUnix atomic.Int64
	elapsedNanos  atomic.Int64 // finalised at odPhaseDone

	archivesTotal       atomic.Int32 // groups, NOT files
	archivesNeedRegen   atomic.Int32
	archivesRegenedDone atomic.Int32
	archivesSkipped     atomic.Int32

	// on-disk file count across all runs. 2 runs of 8 parts = 2 / 16.
	// shown when different from archive count so `ls` matches TUI
	filesTotal atomic.Int32

	regenBytesTotal atomic.Int64 // sum of archive sizes needing regen
	regenBytesRead  atomic.Int64

	// part-level progress, finer than archivesRegenedDone. partsRegenDone
	// ticks visibly on long single-archive multi-part runs where
	// archivesRegenedDone only flips 0->1 at the very end
	partsRegenTotal atomic.Int32
	partsRegenDone  atomic.Int32

	keysTotalEstimate atomic.Int64
	keysLoaded        atomic.Int64

	// per-goroutine status table for the TUI worker rows. each worker
	// only writes its own slot. slice header guarded b/c TUI reads while
	// regenParts publishes/clears
	workersMu sync.RWMutex
	workers   []workerStatus
}

// one regen worker's current state, atomic so reader/writer dont sync.
// archivePath uses atomic.Pointer[string] (no atomic string primitive),
// fresh *string per archive so readers always see a consistent snapshot
type workerStatus struct {
	archivePath atomic.Pointer[string]
	partIdx     atomic.Int32
	partsTotal  atomic.Int32
	bytesDone   atomic.Int64
	bytesTotal  atomic.Int64
}

// non-idle worker slots up to max, fresh slice each call so renderer
// doesnt re-load the atomic
func (m *odMetrics) activeWorkers(max int) []*workerStatus {
	if m == nil {
		return nil
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	if len(m.workers) == 0 {
		return nil
	}
	out := make([]*workerStatus, 0, min(max, len(m.workers)))
	for i := range m.workers {
		if m.workers[i].archivePath.Load() == nil {
			continue
		}
		out = append(out, &m.workers[i])
		if len(out) >= max {
			break
		}
	}
	return out
}

func (m *odMetrics) setWorkerSlots(n int) {
	if m == nil {
		return
	}
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	m.workers = make([]workerStatus, n)
}

func (m *odMetrics) clearWorkerSlots() {
	if m == nil {
		return
	}
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	for i := range m.workers {
		m.workers[i].archivePath.Store(nil)
	}
	m.workers = nil
}

func (m *odMetrics) workerSlot(idx int) *workerStatus {
	if m == nil {
		return nil
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	if idx < 0 || idx >= len(m.workers) {
		return nil
	}
	return &m.workers[idx]
}

func (m *odMetrics) workerCount() int {
	if m == nil {
		return 0
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	return len(m.workers)
}

// one .zst file from a prior run, multi-part runs aggregate several
type archivePart struct {
	path    string
	partNum int   // 0 = single-archive, 1..N for multi-part
	size    int64 // for regen denominators
	modTime time.Time

	// per-part .idx location. per-part sidecars mean stale detection,
	// regen, and load all operate at part granularity w/ no cross-part coord
	sidecarPath string
}

// all parts of one logical sfu run. runID grouping is for DISPLAY only,
// storage and regen are per-part
type archiveRun struct {
	runID string        // "sfu_20260514_xyz"
	parts []archivePart // sorted by partNum asc
}

// captures runID stem + optional _partN. anchored both ends so foreign
// .zst files cant slip through
var archiveNameRE = regexp.MustCompile(`^(sfu_[^/]+?)(?:_part(\d+))?\.txt\.zst$`)

// (runID, partNum) from a filename, ("", 0) on no match
func parseArchiveName(path string) (runID string, partNum int) {
	base := filepath.Base(path)
	m := archiveNameRE.FindStringSubmatch(base)
	if m == nil {
		return "", 0
	}
	runID = m[1]
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0
		}
		partNum = n
	}
	return runID, partNum
}

// globs dir for sfu_*.txt.zst, groups by runID, sorts parts asc.
// excludeRunID (current run) is skipped so we dont dedup against ourselves.
// explicit Stat surfaces "permission denied" as fatal instead of masking it
// as "library is empty" (silent no-dedup is the worst failure mode)
func discoverArchiveRuns(dir, excludeRunID string) ([]archiveRun, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("dest dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("dest dir: %s is not a directory", dir)
	}
	pattern := filepath.Join(dir, "sfu_*.txt.zst")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	runs := make(map[string]*archiveRun)
	for _, p := range matches {
		runID, partNum := parseArchiveName(p)
		if runID == "" {
			continue
		}
		if runID == excludeRunID {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			// race against concurrent dir activity, next run picks it up
			continue
		}
		part := archivePart{
			path:        p,
			partNum:     partNum,
			size:        fi.Size(),
			modTime:     fi.ModTime(),
			sidecarPath: sidecarPathForArchive(p),
		}
		a, ok := runs[runID]
		if !ok {
			a = &archiveRun{runID: runID}
			runs[runID] = a
		}
		a.parts = append(a.parts, part)
	}

	out := make([]archiveRun, 0, len(runs))
	for _, a := range runs {
		sort.Slice(a.parts, func(i, j int) bool {
			return a.parts[i].partNum < a.parts[j].partNum
		})
		out = append(out, *a)
	}
	// deterministic order for debug logs and integration test snapshots
	sort.Slice(out, func(i, j int) bool { return out[i].runID < out[j].runID })
	return out, nil
}

// deletes .idx files in idxSubdirName w/o a live archive part. once per
// phase 0. non-fatal: stray .idx is harmless but tidy keeps the subdir small
func sweepOrphanedSidecars(destDir string, runs []archiveRun) (int, error) {
	subdir := filepath.Join(destDir, idxSubdirName)
	entries, err := os.ReadDir(subdir)
	if err != nil {
		if os.IsNotExist(err) {
			// first -od run on this dir
			return 0, nil
		}
		return 0, err
	}

	live := make(map[string]bool, 32)
	for _, r := range runs {
		for _, p := range r.parts {
			live[filepath.Base(p.sidecarPath)] = true
		}
	}

	removed := 0
	for _, e := range entries {
		name := e.Name()
		// only .idx, leave .tmp / README / etc. alone
		if !strings.HasSuffix(name, sidecarSuffix) {
			continue
		}
		if live[name] {
			continue
		}
		_ = os.Remove(filepath.Join(subdir, name))
		removed++
	}
	return removed, nil
}

// same as above but for search sidecars in sfu_search_idx/
func sweepOrphanedSearchSidecars(destDir string, runs []archiveRun) (int, error) {
	subdir := filepath.Join(destDir, searchidx.SubdirName)
	entries, err := os.ReadDir(subdir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	live := make(map[string]bool, 32)
	for _, r := range runs {
		for _, p := range r.parts {
			live[filepath.Base(searchSidecarPathForArchive(p.path))] = true
		}
	}

	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, searchidx.SidecarSuffix) {
			continue
		}
		if live[name] {
			continue
		}
		_ = os.Remove(filepath.Join(subdir, name))
		removed++
	}
	return removed, nil
}

type sidecarStatus int

const (
	sidecarStatusFresh   sidecarStatus = iota // header valid + mtime >= parts
	sidecarStatusMissing                      // no sidecar on disk
	sidecarStatusStale                        // malformed, wrong version, or older than parts
)

// status of one part's sidecar. reads header but doesnt scan body.
// returns header on fresh status so caller sums keyCount w/o reopen
func classifyPartSidecar(part archivePart) (sidecarStatus, *sidecarHeader) {
	hdr, err := readSidecarHeader(part.sidecarPath)
	if err != nil {
		switch {
		case errors.Is(err, errSidecarMissing):
			return sidecarStatusMissing, nil
		case errors.Is(err, errSidecarMalformed), errors.Is(err, errSidecarStale):
			return sidecarStatusStale, nil
		default:
			// unknown err = treat as missing so we regen rather than
			// surfacing a phase-0 fatal users cant fix
			return sidecarStatusMissing, nil
		}
	}

	// archive newer than sidecar = stale. fires when user manually
	// rebuilt the archive
	si, err := os.Stat(part.sidecarPath)
	if err != nil {
		return sidecarStatusStale, hdr
	}
	if part.modTime.After(si.ModTime()) {
		return sidecarStatusStale, hdr
	}
	return sidecarStatusFresh, hdr
}

type odConfig struct {
	Dest            string
	CurrentRunStamp string // matching files excluded from discovery
	Buckets         int    // MUST match phase 1/2 B
	TempDir         string
	Workers         int // 0 = min(GOMAXPROCS, archivesNeedingRegen)
	Debug           *debugLog
}

// deferred part of the phase-0 contract: populated by routeAllSidecars
// IntoBuckets after a bg goroutine finishes streaming sidecars into per-
// bucket files. surfaced via channel so pipeline can start phase 1 in parallel
type odLoadResult struct {
	DestKeyBucketPaths []string
	TotalKeysLoaded    uint64
	Elapsed            time.Duration
	Err                error
}

type odResult struct {
	DestKeyBucketPaths []string // [bucketIdx] -> path, "" if empty. valid only after loadCh signals
	ArchivesTotal      int
	FilesTotal         int
	ArchivesFresh      int
	ArchivesRegen      int
	ArchivesSkipped    int
	// user-visible skipped paths re-emitted post-alt-screen
	SkippedArchivePaths []string
	TotalKeysLoaded     uint64
	Elapsed             time.Duration
}

// returns:
//   - *odResult w/ regen-time fields. DestKeyBucketPaths/TotalKeysLoaded
//     valid only after receiving from loadCh
//   - loadCh: 1-buffered chan w/ deferred load result. nil if nothing to load.
//     caller MUST receive before using bucket paths
//   - err: synchronous discover/regen failure
//
// deferred-load lets phase 1 run in parallel with library load
func runODScan(ctx context.Context, cfg odConfig, m *odMetrics) (*odResult, <-chan odLoadResult, error) {
	if cfg.Buckets <= 0 {
		return nil, nil, fmt.Errorf("odScan: buckets must be > 0")
	}
	if cfg.Dest == "" {
		return nil, nil, fmt.Errorf("odScan: dest is empty")
	}

	started := time.Now()
	if m != nil {
		m.phase.Store(int32(odPhaseDiscover))
		m.startedAtUnix.Store(started.Unix())
	}

	runs, err := discoverArchiveRuns(cfg.Dest, cfg.CurrentRunStamp)
	if err != nil {
		return nil, nil, fmt.Errorf("odScan: discover: %w", err)
	}
	cfg.Debug.Event("[od] scan begin: dest=%s, found=%d runs", cfg.Dest, len(runs))

	// orphan sweep, non-fatal
	if n, err := sweepOrphanedSidecars(cfg.Dest, runs); err != nil {
		cfg.Debug.Event("[od] orphan sweep: warn: %v", err)
	} else if n > 0 {
		cfg.Debug.Event("[od] orphan sweep: removed %d stale .idx files", n)
	}
	if n, err := sweepOrphanedSearchSidecars(cfg.Dest, runs); err != nil {
		cfg.Debug.Event("[od] search orphan sweep: warn: %v", err)
	} else if n > 0 {
		cfg.Debug.Event("[od] search orphan sweep: removed %d stale .sfsidx.json files", n)
	}

	if len(runs) == 0 {
		if m != nil {
			m.phase.Store(int32(odPhaseDone))
			m.elapsedNanos.Store(int64(time.Since(started)))
		}
		cfg.Debug.Event("[od] scan: no prior archives, skipping phase 0")
		return &odResult{Elapsed: time.Since(started)}, nil, nil
	}

	// per-part classify: touching one part of a 16-part run invalidates
	// only that part, siblings stay fresh
	var needRegen []archivePart
	var regenBytes int64
	var keysEstimate int64
	var totalParts int
	for _, r := range runs {
		for _, p := range r.parts {
			totalParts++
			st, hdr := classifyPartSidecar(p)
			switch st {
			case sidecarStatusFresh:
				keysEstimate += int64(hdr.keyCount)
			case sidecarStatusMissing, sidecarStatusStale:
				needRegen = append(needRegen, p)
				regenBytes += p.size
				// conservative pre-regen estimate: ~10 B archive per credential
				// (ULP ~50 B/line @ ~5x zstd ratio). refined post-regen
				keysEstimate += p.size / 10
			}
		}
	}

	if m != nil {
		m.archivesTotal.Store(int32(len(runs)))
		// runs w/ at least one dirty part, for display continuity
		runsWithDirtyPart := countRunsWithDirtyPart(runs, needRegen)
		m.archivesNeedRegen.Store(int32(runsWithDirtyPart))
		m.regenBytesTotal.Store(regenBytes)
		m.keysTotalEstimate.Store(keysEstimate)
		m.filesTotal.Store(int32(totalParts))
		m.partsRegenTotal.Store(int32(len(needRegen)))
	}
	cfg.Debug.Event("[od] scan classify: parts_total=%d, parts_need_regen=%d, regen_bytes=%d",
		totalParts, len(needRegen), regenBytes)

	// regen pool, one task per part. corrupt part = skip w/ warning,
	// siblings stay usable
	skippedParts := make(map[string]bool)
	var skippedPartPaths []string
	if len(needRegen) > 0 {
		if m != nil {
			m.phase.Store(int32(odPhaseRegen))
		}
		paths, err := regenParts(ctx, needRegen, cfg, m)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range paths {
			skippedParts[p] = true
		}
		skippedPartPaths = paths
		if m != nil {
			m.archivesSkipped.Store(int32(len(paths)))
		}
	}

	// majority-skipped check: > half the library unreadable = refuse run.
	// silent half-library dedup is worse than failing fast
	totalSkippedParts := len(skippedParts)
	if totalParts > 0 && totalSkippedParts*2 > totalParts {
		return nil, nil, fmt.Errorf(
			"od-scan: %d of %d archive parts in %s were unreadable (%d%%). "+
				"This is too many to safely dedup against -- check the directory and try again",
			totalSkippedParts, totalParts, cfg.Dest,
			(totalSkippedParts*100)/totalParts)
	}

	// run-level counts: fresh = all parts fresh, regen = at least one
	// dirty and all succeeded, skipped = at least one failed
	runsFresh, runsRegen, runsSkipped, skippedRunPaths := classifyRunOutcomes(runs, needRegen, skippedParts)

	res := &odResult{
		ArchivesTotal:       len(runs),
		ArchivesFresh:       runsFresh,
		ArchivesRegen:       runsRegen,
		ArchivesSkipped:     runsSkipped,
		SkippedArchivePaths: skippedRunPaths,
		FilesTotal:          totalParts,
		Elapsed:             time.Since(started), // refined by load goroutine
	}
	_ = skippedPartPaths // surfaced via skippedRunPaths

	// load list = all parts minus the skipped ones. keep healthy siblings
	// of a partially-corrupt run
	loadParts := make([]archivePart, 0, totalParts)
	for _, r := range runs {
		for _, p := range r.parts {
			if skippedParts[p.path] {
				continue
			}
			loadParts = append(loadParts, p)
		}
	}

	if len(loadParts) == 0 {
		// every part corrupt and skipped, majority check above already
		// validated this isnt "too many bad"
		if m != nil {
			m.phase.Store(int32(odPhaseDone))
			m.elapsedNanos.Store(int64(res.Elapsed))
		}
		cfg.Debug.Event("[od] scan done: no readable archive parts to load")
		return res, nil, nil
	}

	// refresh keysTotalEstimate w/ real sidecar headers now regen is done
	if m != nil {
		var totalKeys int64
		for _, p := range loadParts {
			if hdr, err := readSidecarHeader(p.sidecarPath); err == nil {
				totalKeys += int64(hdr.keyCount)
			}
		}
		if totalKeys > 0 {
			m.keysTotalEstimate.Store(totalKeys)
		}
	}

	// load runs in bg, phase 1 can run while this completes.
	// 1-buffered so goroutine never blocks if caller abandons
	loadCh := make(chan odLoadResult, 1)
	if m != nil {
		m.phase.Store(int32(odPhaseLoad))
	}
	go func() {
		loadStart := time.Now()
		loadRes, err := routeAllSidecarsIntoBuckets(ctx, loadParts, cfg, m)
		if err != nil {
			loadCh <- odLoadResult{Err: fmt.Errorf("odScan: route: %w", err)}
			return
		}
		elapsed := time.Since(started)
		if m != nil {
			m.phase.Store(int32(odPhaseDone))
			m.elapsedNanos.Store(int64(elapsed))
		}
		cfg.Debug.Event("[od] scan done: archives=%d, fresh=%d, regen=%d, skipped=%d, keys_loaded=%d, load_elapsed=%s, total_elapsed=%s",
			res.ArchivesTotal, res.ArchivesFresh, res.ArchivesRegen, res.ArchivesSkipped,
			loadRes.TotalKeysLoaded, time.Since(loadStart), elapsed)
		loadCh <- odLoadResult{
			DestKeyBucketPaths: loadRes.DestKeyBucketPaths,
			TotalKeysLoaded:    loadRes.TotalKeysLoaded,
			Elapsed:            elapsed,
		}
	}()

	return res, loadCh, nil
}

// runs w/ at least one part in needRegen. O(R*P), inputs bounded by dir listing
func countRunsWithDirtyPart(runs []archiveRun, needRegen []archivePart) int {
	if len(needRegen) == 0 {
		return 0
	}
	dirty := make(map[string]bool, len(needRegen))
	for _, p := range needRegen {
		dirty[p.path] = true
	}
	count := 0
	for _, r := range runs {
		for _, p := range r.parts {
			if dirty[p.path] {
				count++
				break
			}
		}
	}
	return count
}

// per-run counts from per-part regen result. fresh = no dirty parts,
// regen = at least one dirty AND no skipped, skipped = at least one skipped.
// skipped runs report part-1 path for orientation
func classifyRunOutcomes(runs []archiveRun, needRegen []archivePart, skippedParts map[string]bool) (fresh, regen, skipped int, skippedPaths []string) {
	dirty := make(map[string]bool, len(needRegen))
	for _, p := range needRegen {
		dirty[p.path] = true
	}
	for _, r := range runs {
		anyDirty := false
		anySkipped := false
		for _, p := range r.parts {
			if dirty[p.path] {
				anyDirty = true
			}
			if skippedParts[p.path] {
				anySkipped = true
			}
		}
		switch {
		case anySkipped:
			skipped++
			if len(r.parts) > 0 {
				skippedPaths = append(skippedPaths, r.parts[0].path)
			}
		case anyDirty:
			regen++
		default:
			fresh++
		}
	}
	return
}

// one part to stream + hash + finalise. no cross-task coord
type partTask struct {
	part archivePart
}

// parallel per-part regen. each task streams its archive, hashes every
// parseable credential into its own .idx, then atomically finalises.
// no aggregation, sidecars are self-contained. corrupt parts return in
// the skipped list, siblings stay usable
func regenParts(ctx context.Context, parts []archivePart, cfg odConfig, m *odMetrics) ([]string, error) {
	if len(parts) == 0 {
		return nil, nil
	}

	workers := cfg.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > len(parts) {
		workers = len(parts)
	}
	if workers < 1 {
		workers = 1
	}

	// adaptive decoder concurrency: single huge archive gets all
	// GOMAXPROCS decoder goroutines on that stream, many small archives
	// collapse per-stream fan-out to 1. total decoder pop stays ~GOMAXPROCS
	decoderConcurrency := runtime.GOMAXPROCS(0) / workers
	if decoderConcurrency < 1 {
		decoderConcurrency = 1
	}
	cfg.Debug.Event("[od] regen pool: workers=%d decoder_concurrency_per_archive=%d (GOMAXPROCS=%d, parts=%d)",
		workers, decoderConcurrency, runtime.GOMAXPROCS(0), len(parts))

	if m != nil {
		m.setWorkerSlots(workers)
		defer m.clearWorkerSlots()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	taskCh := make(chan partTask)
	errCh := make(chan error, workers)

	var skippedMu sync.Mutex
	var skippedPaths []string

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		workerIdx := i
		go func() {
			defer wg.Done()
			var ws *workerStatus
			if m != nil {
				ws = m.workerSlot(workerIdx)
			}
			if ws != nil {
				defer ws.archivePath.Store(nil)
			}
			fmtr := newLineFormatter()
			for t := range taskCh {
				select {
				case <-ctx.Done():
					return
				default:
				}

				started := time.Now()
				keys, err := processPartTask(ctx, t, decoderConcurrency, ws, fmtr, m)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					if !errors.Is(err, errCorruptArchive) {
						cfg.Debug.Event("[od] regen FATAL: part=%s, err=%v",
							filepath.Base(t.part.path), err)
						select {
						case errCh <- fmt.Errorf("regen %s: %w", filepath.Base(t.part.path), err):
							cancel()
						default:
						}
						return
					}
					fmt.Fprintf(os.Stderr, "sfu: warning: skipping corrupt archive part %s: %v\n",
						t.part.path, err)
					skippedMu.Lock()
					skippedPaths = append(skippedPaths, t.part.path)
					skippedMu.Unlock()
					cfg.Debug.Event("[od] regen SKIP (corrupt): part=%s, err=%v",
						filepath.Base(t.part.path), err)
				} else {
					cfg.Debug.Event("[od] regen done: part=%s, keys=%d, elapsed=%s",
						filepath.Base(t.part.path), keys, time.Since(started))
				}

				if m != nil {
					m.partsRegenDone.Add(1)
					if err == nil {
						m.archivesRegenedDone.Add(1)
					}
				}
			}
		}()
	}

	go func() {
		defer close(taskCh)
		for _, p := range parts {
			select {
			case taskCh <- partTask{part: p}:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	if e, ok := <-errCh; ok && e != nil {
		return nil, e
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return skippedPaths, nil
}

// streams one part, hashes lines into its temp .idx, atomic finalise.
// errCorruptArchive = skip w/ warning, other errs propagate fatal
func processPartTask(ctx context.Context, t partTask, decoderConcurrency int, ws *workerStatus, fmtr *lineFormatter, m *odMetrics) (uint64, error) {
	// publish "working on X" BEFORE open so TUI never sees partial state.
	// partIdx/partsTotal kept for TUI continuity, both 1 means rows show
	// just the filename
	if ws != nil {
		name := filepath.Base(t.part.path)
		ws.archivePath.Store(&name)
		ws.partIdx.Store(1)
		ws.partsTotal.Store(1)
		ws.bytesDone.Store(0)
		ws.bytesTotal.Store(t.part.size)
	}

	sw, err := newSidecarWriter(t.part.path)
	if err != nil {
		return 0, err
	}

	streamErr := streamArchiveLines(ctx, t.part.path, decoderConcurrency, ws, func(line string) error {
		host, _, login, password, ok := parseFor(line, true)
		if !ok {
			return nil
		}
		h := fmtr.HashKey(host, login, password)
		return sw.WriteHash(h)
	}, m)
	if streamErr != nil {
		_ = sw.Abort()
		return 0, streamErr
	}

	keys, err := sw.Commit()
	if err != nil {
		return 0, fmt.Errorf("sidecar write: %w", err)
	}
	return keys, nil
}

// stamps .idx for this run's own output by re-streaming each part.
// reuses regenParts so theres ONE sidecar-producing code path.
// 60s serial pass becomes ~4s on 16 cores/parts, and the user sees a real bar
func regenOwnOutputSidecars(ctx context.Context, outputPaths []string, dbg *debugLog, m *odMetrics) error {
	if len(outputPaths) == 0 {
		return nil
	}

	parts := make([]archivePart, 0, len(outputPaths))
	var totalBytes int64
	for _, path := range outputPaths {
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat output %s: %w", path, err)
		}
		parts = append(parts, archivePart{
			path:        path,
			size:        fi.Size(),
			modTime:     fi.ModTime(),
			sidecarPath: sidecarPathForArchive(path),
		})
		totalBytes += fi.Size()
	}

	if m != nil {
		// 1 logical archive (this run), N parts. matches foreign-scan convention
		m.archivesTotal.Store(1)
		m.filesTotal.Store(int32(len(parts)))
		m.archivesNeedRegen.Store(1)
		m.partsRegenTotal.Store(int32(len(parts)))
		m.regenBytesTotal.Store(totalBytes)
		m.phase.Store(int32(odPhaseIndexOwn))
		m.startedAtUnix.Store(time.Now().Unix())
	}

	started := time.Now()
	skipped, err := regenParts(ctx, parts, odConfig{Debug: dbg}, m)
	if err != nil {
		return fmt.Errorf("output sidecar regen: %w", err)
	}
	if len(skipped) > 0 {
		// should be impossible, we just wrote these w/ a known-good encoder
		dbg.Event("[od] own-output regen: %d parts skipped as corrupt (unexpected)",
			len(skipped))
	}
	if m != nil {
		m.elapsedNanos.Store(time.Since(started).Nanoseconds())
		m.phase.Store(int32(odPhaseDone))
	}
	dbg.Event("[od] own-output sidecars complete: parts=%d, bytes=%d, elapsed=%s",
		len(parts), totalBytes, time.Since(started))
	return nil
}

// test convenience, single-part w/o the worker pool.
// always parseLoose b/c past archives may include loose-only shapes
// (eg host:port:user:pw, no TLD). loose tries strict first so the cost
// is ~zero on strict-parseable lines
func regenSidecarForPart(ctx context.Context, part archivePart, decoderConcurrency int, ws *workerStatus, m *odMetrics) (uint64, error) {
	if ws != nil {
		defer ws.archivePath.Store(nil)
	}
	fmtr := newLineFormatter()
	return processPartTask(ctx, partTask{part: part}, decoderConcurrency, ws, fmtr, m)
}

// archive-side errors (truncated zstd, malformed compressed data, mid-stream
// abort) vs system errors (EIO, EACCES, ENOSPC). regenParts uses errors.Is
// to decide skip-with-warning vs propagate-fatal
var errCorruptArchive = errors.New("corrupt archive")

// opens path (zstd-decoded if .zst), trims each line, calls fn.
// reports RAW archive bytes (not decoded) so progress bar matches the
// regenBytesTotal denominator.
// decoderConcurrency caps zstd reader goroutines so total decoder pop
// stays ~GOMAXPROCS across the pool. values < 1 clamped.
// zstd-side failures wrapped as errCorruptArchive, plain-text errors propagate raw
func streamArchiveLines(ctx context.Context, path string, decoderConcurrency int, ws *workerStatus, fn func(string) error, m *odMetrics) error {
	if decoderConcurrency < 1 {
		decoderConcurrency = 1
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	counter := &countingReader{r: f}
	var src io.Reader = counter
	// case-insensitive ext: macOS/Windows surface .ZST/.Zst
	if strings.EqualFold(filepath.Ext(path), ".zst") {
		dec, err := zstd.NewReader(counter, zstd.WithDecoderConcurrency(decoderConcurrency))
		if err != nil {
			return fmt.Errorf("%w: zstd reader: %v", errCorruptArchive, err)
		}
		defer dec.Close()
		src = dec
	}

	br := bufio.NewReaderSize(src, 4*1024*1024)
	var lastReportedRaw int64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, consumed, tooLong, rerr := readBoundedLine(br, maxInputLineBytes)
		if consumed > 0 && !tooLong {
			trimmed := line
			// strip trailing CR/LF w/o alloc, fn is read-only
			for len(trimmed) > 0 && (trimmed[len(trimmed)-1] == '\n' || trimmed[len(trimmed)-1] == '\r') {
				trimmed = trimmed[:len(trimmed)-1]
			}
			if trimmed != "" {
				if err := fn(trimmed); err != nil {
					return err
				}
			}
		}
		// counter delta is bursty (bufio refills in 4 MiB jumps) but totals exact
		rawNow := counter.n.Load()
		if delta := rawNow - lastReportedRaw; delta > 0 {
			if m != nil {
				m.regenBytesRead.Add(delta)
			}
			if ws != nil {
				ws.bytesDone.Store(rawNow)
			}
			lastReportedRaw = rawNow
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			// .zst read err = mid-stream zstd failure = corruption.
			// plain text = let it propagate
			if strings.EqualFold(filepath.Ext(path), ".zst") {
				return fmt.Errorf("%w: read: %v", errCorruptArchive, rerr)
			}
			return rerr
		}
	}
}

// one append-only writer per dest bucket file. closed by
// routeAllSidecarsIntoBuckets, empty buckets get their 0-byte files
// removed so phase 2 skips the open()
type destBucketWriters struct {
	paths   []string
	files   []*os.File
	writers []*bufio.Writer
	counts  []uint64
	// per-instance scratch for WriteKey. used to be a package-level var,
	// which was a hidden trap: future parallelization would have raced
	keyBuf [sidecarKeyBytes]byte
}

func newDestBucketWriters(rootDir string, buckets int) (*destBucketWriters, error) {
	dir := filepath.Join(rootDir, "dest_keys")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dw := &destBucketWriters{
		paths:   make([]string, buckets),
		files:   make([]*os.File, buckets),
		writers: make([]*bufio.Writer, buckets),
		counts:  make([]uint64, buckets),
	}
	bufBytes := bucketBufBytes(buckets)
	for i := 0; i < buckets; i++ {
		p := filepath.Join(dir, fmt.Sprintf("bucket_%05d.bin", i))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			dw.abort()
			return nil, err
		}
		dw.paths[i] = p
		dw.files[i] = f
		dw.writers[i] = bufio.NewWriterSize(f, bufBytes)
	}
	return dw, nil
}

// appends k to bucket idx. NOT thread-safe, routing is single-threaded
// (CPU-trivial, parallelism wouldnt buy much)
func (dw *destBucketWriters) WriteKey(idx int, k uint64) error {
	binary.LittleEndian.PutUint64(dw.keyBuf[:], k)
	if _, err := dw.writers[idx].Write(dw.keyBuf[:]); err != nil {
		return err
	}
	dw.counts[idx]++
	return nil
}

// flushes + closes every writer, empty bucket files removed.
// returns first error but always attempts every close.
// nils file slots on success so a deferred abort() after a late caller
// error cant remove the files we just produced
func (dw *destBucketWriters) Close() error {
	var firstErr error
	for i, w := range dw.writers {
		if w != nil {
			if err := w.Flush(); err != nil && firstErr == nil {
				firstErr = err
			}
			dw.writers[i] = nil
		}
		if f := dw.files[i]; f != nil {
			if err := f.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
			dw.files[i] = nil
		}
		if dw.counts[i] == 0 {
			_ = os.Remove(dw.paths[i])
			dw.paths[i] = ""
		}
	}
	return firstErr
}

// error-path cleanup. files Close() already committed have files[i]==nil
// and paths[i]!="" so abort LEAVES THOSE ALONE
func (dw *destBucketWriters) abort() {
	for i, f := range dw.files {
		if f == nil {
			continue
		}
		_ = f.Close()
		dw.files[i] = nil
		if dw.paths[i] != "" {
			_ = os.Remove(dw.paths[i])
			dw.paths[i] = ""
		}
	}
}

const sidecarRouteChunkBytes = 4 * 1024 * 1024

// streams every per-part sidecar in multi-MiB chunks, writes each key
// into its hash-modulo-B bucket. single-threaded by design (parallel
// routing needs a diff writer architecture)
func routeAllSidecarsIntoBuckets(ctx context.Context, parts []archivePart, cfg odConfig, m *odMetrics) (*odResult, error) {
	dw, err := newDestBucketWriters(cfg.TempDir, cfg.Buckets)
	if err != nil {
		return nil, fmt.Errorf("newDestBucketWriters: %w", err)
	}
	defer func() {
		if dw != nil {
			dw.abort()
		}
	}()

	usePow2 := cfg.Buckets > 0 && (cfg.Buckets&(cfg.Buckets-1)) == 0
	mask := uint64(cfg.Buckets) - 1

	var totalKeys uint64
	var routeSkipped int
	for _, p := range parts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		err := streamSidecarKeyBytes(p.sidecarPath, sidecarRouteChunkBytes, func(raw []byte) error {
			for off := 0; off < len(raw); off += sidecarKeyBytes {
				k := binary.LittleEndian.Uint64(raw[off : off+sidecarKeyBytes])
				idx := bucketIndex(k, mask, usePow2, cfg.Buckets)
				if err := dw.WriteKey(int(idx), k); err != nil {
					return err
				}
				totalKeys++
				if m != nil {
					// per-key atomic would dominate, update once per 8K keys
					// (~64 KiB, matches bufio flush cadence)
					if totalKeys%8192 == 0 {
						m.keysLoaded.Store(int64(totalKeys))
					}
				}
			}
			return nil
		})
		if err != nil {
			// narrow window: concurrent process bumped parserVersion or
			// corrupted sidecar between classify and route. drop the part
			// from dedup, warn. better UX than fatal mid-phase-1
			if errors.Is(err, errSidecarStale) || errors.Is(err, errSidecarMalformed) {
				routeSkipped++
				if m != nil {
					m.archivesSkipped.Add(1)
				}
				fmt.Fprintf(os.Stderr,
					"sfu: warning: dropping archive part from dedup set; sidecar became stale or malformed mid-run: %s (%v)\n",
					p.path, err)
				cfg.Debug.Event("[od] route skip: part=%s reason=%v", filepath.Base(p.path), err)
				continue
			}
			return nil, fmt.Errorf("route sidecar %s: %w", p.sidecarPath, err)
		}
		cfg.Debug.Event("[od] sidecar loaded: part=%s, keys_so_far=%d", filepath.Base(p.path), totalKeys)
	}

	if m != nil {
		m.keysLoaded.Store(int64(totalKeys))
		m.phase.Store(int32(odPhaseCommitBuckets))
	}
	cfg.Debug.Event("[od] route commit: keys=%d buckets=%d", totalKeys, cfg.Buckets)
	if err := dw.Close(); err != nil {
		return nil, err
	}

	// hand paths to caller, nil our pointer so deferred abort() is a no-op
	out := &odResult{
		DestKeyBucketPaths: dw.paths,
		TotalKeysLoaded:    totalKeys,
	}
	dw = nil
	return out, nil
}
