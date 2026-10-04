package ulpengine

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/snowx-dev/SnowFastULP/internal/zstdlimits"

	"github.com/klauspost/compress/zstd"
)

// phase 0 (-od): discover prior sfu_*.txt.zst archives at dest and regen
// missing/stale .idx sidecars (legacy v2/v3 sidecars regenerate too: they
// carry no archive-identity binding, H-21). dedup (phase 2) then reads each
// bucket's library keys directly from the sorted sidecars
// the sorted sidecars (top-bits range reads), one sidecar open at a time, so
// RAM and fd use stay bounded regardless of library size. no per-run routing.

// 0 = idle, the 2nd TUI frame renders iff phase != idle && phase != done
type odPhase int32

const (
	odPhaseIdle     odPhase = 0
	odPhaseDiscover odPhase = 1
	odPhaseRegen    odPhase = 2 // decompress archives + write fresh .idx
	odPhaseDone     odPhase = 3
	odPhaseUpgrade  odPhase = 4 // in-place v2->v3 sidecar re-sort (no archive read)
)

func odPhaseInFlight(m *ODMetrics) bool {
	if m == nil {
		return false
	}
	ph := odPhase(m.Phase.Load())
	return ph != odPhaseIdle && ph != odPhaseDone
}

// atomic counters for the TUI's second frame + end-of-run summary.
// lock-free so TUI doesnt coordinate w/ phase-0 workers
type ODMetrics struct {
	Phase         atomic.Int32
	startedAtUnix atomic.Int64
	elapsedNanos  atomic.Int64 // finalised at odPhaseDone

	ArchivesTotal       atomic.Int32 // groups, NOT files
	ArchivesNeedRegen   atomic.Int32
	ArchivesRegenedDone atomic.Int32
	ArchivesSkipped     atomic.Int32

	// on-disk file count across all runs. 2 runs of 8 parts = 2 / 16.
	// shown when different from archive count so `ls` matches TUI
	FilesTotal atomic.Int32

	RegenBytesTotal atomic.Int64 // sum of archive sizes needing regen
	RegenBytesRead  atomic.Int64

	// part-level progress, finer than archivesRegenedDone. partsRegenDone
	// ticks visibly on long single-archive multi-part runs where
	// archivesRegenedDone only flips 0->1 at the very end
	PartsRegenTotal atomic.Int32
	PartsRegenDone  atomic.Int32

	// set during discover when legacy v2 sidecars need a one-time in-place upgrade
	PartsUpgradeTotal atomic.Int32

	KeysTotalEstimate atomic.Int64
	KeysLoaded        atomic.Int64

	// per-goroutine status table for the TUI worker rows. each worker
	// only writes its own slot. slice header guarded b/c TUI reads while
	// regenParts publishes/clears
	workersMu sync.RWMutex
	Workers   []WorkerStatus
}

// one regen worker's current state, atomic so reader/writer dont sync.
// archivePath uses atomic.Pointer[string] (no atomic string primitive),
// fresh *string per archive so readers always see a consistent snapshot
type WorkerStatus struct {
	ArchivePath atomic.Pointer[string]
	PartIdx     atomic.Int32
	PartsTotal  atomic.Int32
	BytesDone   atomic.Int64
	BytesTotal  atomic.Int64
	// true while the part's sidecar commits (spill merge + fsync + rename):
	// BytesDone already reached BytesTotal at stream EOF, but the part only
	// counts toward PartsRegenDone after the commit lands, so the TUI must
	// know the row is still working
	Committing atomic.Bool
}

// non-idle worker slots up to max, fresh slice each call so renderer
// doesnt re-load the atomic
func (m *ODMetrics) ActiveWorkers(max int) []*WorkerStatus {
	if m == nil {
		return nil
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	if len(m.Workers) == 0 {
		return nil
	}
	out := make([]*WorkerStatus, 0, min(max, len(m.Workers)))
	for i := range m.Workers {
		if m.Workers[i].ArchivePath.Load() == nil {
			continue
		}
		out = append(out, &m.Workers[i])
		if len(out) >= max {
			break
		}
	}
	return out
}

func (m *ODMetrics) setWorkerSlots(n int) {
	if m == nil {
		return
	}
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	m.Workers = make([]WorkerStatus, n)
}

func (m *ODMetrics) clearWorkerSlots() {
	if m == nil {
		return
	}
	m.workersMu.Lock()
	defer m.workersMu.Unlock()
	for i := range m.Workers {
		m.Workers[i].ArchivePath.Store(nil)
	}
	m.Workers = nil
}

func (m *ODMetrics) workerSlot(idx int) *WorkerStatus {
	if m == nil {
		return nil
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	if idx < 0 || idx >= len(m.Workers) {
		return nil
	}
	return &m.Workers[idx]
}

func (m *ODMetrics) WorkerCount() int {
	if m == nil {
		return 0
	}
	m.workersMu.RLock()
	defer m.workersMu.RUnlock()
	return len(m.Workers)
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

	// optional regen output override (M-06 dry-run): when set, a regen writes
	// the rebuilt .idx here instead of sidecarPath, while the header still
	// binds to `path` (archiveSource). Empty = write the real sidecar.
	sidecarOutPath string
}

// all parts of one logical sfu run. runID grouping is for DISPLAY only,
// storage and regen are per-part
type archiveRun struct {
	runID string        // "sfu_20260514_xyz"
	parts []archivePart // sorted by partNum asc
}

// dry-run regen output dir under the run temp dir (M-06): rebuilt .idx
// copies live here for the run, never in the library.
const dryRunRegenDirName = "odregen"

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

// statArchive is the seam for statting a discovered library archive entry;
// tests inject failures (EACCES, I/O) that real concurrent activity would
// not produce.
var statArchive = os.Stat

// statSweepArchive is the seam for the orphan sweep's archive re-stat (the
// "is this sidecar's archive still there" check); tests inject non-ENOENT
// failures to pin the retain-on-error policy.
var statSweepArchive = os.Stat

// listLibraryArchives reads dir and returns the paths of regular entries
// whose basename parses as an sfu archive. os.ReadDir carries no pattern
// semantics, so metacharacters in a user-controlled library path ([vault],
// *, ?) are literal data, not glob syntax — the old filepath.Glob silently
// discovered nothing under a "[vault]" directory. Entries are returned in
// ReadDir's lexical order (matching the old Glob ordering); symlinks and
// directories are filtered, leaving regular archive files only.
func listLibraryArchives(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if runID, _ := parseArchiveName(e.Name()); runID == "" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// reads dir for sfu archives, groups by runID, sorts parts asc.
// excludeRunID (current run) is skipped so we dont dedup against ourselves.
// the dir Stat and the per-archive Stat surface "permission denied" and
// other real failures as fatal instead of masking them as "library is
// empty" (silent no-dedup is the worst failure mode)
func discoverArchiveRuns(dir, excludeRunID string) ([]archiveRun, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("dest dir: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("dest dir: %s is not a directory", dir)
	}
	matches, err := listLibraryArchives(dir)
	if err != nil {
		return nil, fmt.Errorf("read dest dir: %w", err)
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
		fi, err := statArchive(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// benign deletion race: archive vanished between glob and
				// stat, next run picks up fresh state
				continue
			}
			return nil, fmt.Errorf("stat dest archive %s: %w", p, err)
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
// phase 0. non-fatal: stray .idx is harmless but tidy keeps the subdir small.
// the archive re-stat fails closed: ENOENT deletes the orphan sidecar, any
// other stat error (EACCES/EIO/ESTALE on a flaky or network-mounted library)
// retains it — an unreadable archive is not evidence it is gone — and logs a
// debug event; the next phase-0 sweep revisits.
func sweepOrphanedSidecars(destDir string, runs []archiveRun, dbg *DebugLog) (int, error) {
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
		if strings.HasSuffix(name, sidecarSuffix) {
			if live[name] {
				continue
			}
			// re-stat the referenced archive before deleting: the lock
			// serializes our own runs, but a manual/foreign change could have
			// added (or renamed to) this archive after our dir listing. the
			// sidecar name is "<archiveBase>.txt.zst.idx".
			archivePath := filepath.Join(destDir, strings.TrimSuffix(name, sidecarSuffix))
			_, serr := statSweepArchive(archivePath)
			if serr == nil {
				continue
			}
			if !errors.Is(serr, fs.ErrNotExist) {
				// EACCES/EIO/ESTALE/... on a temporarily unreadable mount is
				// not evidence the archive is gone; deleting here could
				// destroy a valid sidecar. Retain it and revisit next pass.
				dbg.Event("[od] orphan sweep: retained %s: archive stat failed: %v", name, serr)
				continue
			}
			_ = os.Remove(filepath.Join(subdir, name))
			removed++
			continue
		}
		// stale .write/.idxrun spill temps from a hard-killed run (signal cleanup
		// handles graceful exits). age-gated so a concurrent run's live temp is
		// never removed mid-flight.
		if strings.HasSuffix(name, ".tmp") {
			full := filepath.Join(subdir, name)
			if fi, err := os.Stat(full); err == nil && time.Since(fi.ModTime()) > staleTempDirAge {
				_ = os.Remove(full)
				removed++
			}
		}
	}
	return removed, nil
}

// same as above but for search sidecars in sfu_search_idx/. Same fail-closed
// re-stat policy: ENOENT deletes, any other stat error retains the sidecar
// and logs a debug event.
func sweepOrphanedSearchSidecars(destDir string, runs []archiveRun, dbg *DebugLog) (int, error) {
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
		// same re-stat defense as the .idx sweep: skip if the archive
		// reappeared after our dir listing. name is "<archiveBase>.txt.zst.sfsidx.json".
		archivePath := filepath.Join(destDir, strings.TrimSuffix(name, searchidx.SidecarSuffix))
		_, serr := statSweepArchive(archivePath)
		if serr == nil {
			continue
		}
		if !errors.Is(serr, fs.ErrNotExist) {
			// fail closed like the .idx sweep: an unreadable archive is not
			// evidence it is gone; retain the sidecar, revisit next pass.
			dbg.Event("[od] search orphan sweep: retained %s: archive stat failed: %v", name, serr)
			continue
		}
		_ = os.Remove(filepath.Join(subdir, name))
		removed++
	}
	return removed, nil
}

type sidecarStatus int

const (
	sidecarStatusFresh   sidecarStatus = iota // identity-verified against the archive
	sidecarStatusMissing                      // no sidecar on disk
	sidecarStatusStale                        // malformed, legacy (no identity), or archive mismatch
)

// short label for debug lines and dry-run omission reasons
func sidecarStatusName(st sidecarStatus) string {
	switch st {
	case sidecarStatusFresh:
		return "fresh"
	case sidecarStatusMissing:
		return "missing"
	case sidecarStatusStale:
		return "stale"
	default:
		return "unknown"
	}
}

// status of one part's sidecar. reads header but doesnt scan body.
// returns header on fresh status so caller sums keyCount w/o reopen.
// allowVerifyBackfill: after a digest pass confirms the archive, the verify
// cache in the sidecar header is refreshed so later runs skip the hash.
// Dry runs pass false — a preview must not mutate the library.
func classifyPartSidecar(part archivePart, allowVerifyBackfill bool) (sidecarStatus, *sidecarHeader) {
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

	// H-21: bind freshness to the archive's CONTENT, not to mtimes. Size is
	// checked first (cheap), then the verify cache, then — on the first
	// sight of an archive instance whose cheap identity does not yet match —
	// the full SHA-256 recorded in the header. A backup restore or an
	// in-place rewrite with a forged equal/older mtime therefore regenerates
	// instead of silently deduping against stale keys.
	af, err := os.Stat(part.path)
	if err != nil {
		// archive unreadable now: not provably current, fail closed
		return sidecarStatusStale, hdr
	}
	if af.Size() != hdr.archiveSize {
		return sidecarStatusStale, hdr
	}
	mtNano := af.ModTime().UnixNano()
	dev, ino, haveToken := archiveInstanceDevIno(af)
	if hdr.verifyMtimeNano == mtNano && (!haveToken || (hdr.verifyDev == dev && hdr.verifyIno == ino)) {
		// cheap path: a previous digest pass confirmed exactly this archive
		// instance. On platforms without an instance token (Windows) this
		// degrades to size+mtime matching after one digest, the same
		// documented tradeoff as the search index. Residual, shared with the
		// search index: an IN-PLACE rewrite that restores the original mtime
		// keeps the (dev, ino) instance, so the cache accepts it until the
		// mtime actually changes — only a rename-based replacement is always
		// re-hashed.
		return sidecarStatusFresh, hdr
	}

	// first sight of this instance: only the digest can distinguish an
	// equal-mtime rewrite from the content the sidecar was built from.
	ident, ierr := archiveSourceIdentity(part.path)
	if ierr != nil {
		return sidecarStatusStale, hdr
	}
	var want [32]byte
	if _, derr := hex.Decode(want[:], []byte(ident.SHA256)); derr != nil {
		return sidecarStatusStale, hdr
	}
	if want != hdr.sha {
		return sidecarStatusStale, hdr
	}

	// digest confirmed THIS instance: refresh the verify cache so later runs
	// skip the hash. Re-stat first to avoid recording an instance that
	// changed while we hashed (narrowed, not eliminated — same caveat as the
	// search-index backfill).
	if allowVerifyBackfill {
		if post, perr := os.Stat(part.path); perr == nil {
			pmt := post.ModTime().UnixNano()
			pdev, pino, ptok := archiveInstanceDevIno(post)
			if post.Size() == hdr.archiveSize && pmt == mtNano &&
				(!ptok || (pdev == dev && pino == ino)) {
				backfillSidecarVerify(part.sidecarPath, pmt, pdev, pino)
			}
		}
	}
	return sidecarStatusFresh, hdr
}

// backfillSidecarVerify best-effort records a digest-confirmed archive
// instance in the sidecar header's verify cache. The sidecar is derived
// data: a torn or failed rewrite costs one extra digest pass next run
// (readSidecarHeader rejects a short write as malformed and the scan
// regenerates), never a correctness problem.
func backfillSidecarVerify(sidecarPath string, mtNano int64, dev, ino uint64) {
	f, err := os.OpenFile(sidecarPath, os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	var ext [sidecarHeaderBytes - sidecarBaseHeaderBytes]byte
	if _, err := f.ReadAt(ext[:], sidecarBaseHeaderBytes); err != nil {
		return
	}
	binary.LittleEndian.PutUint64(ext[40:48], uint64(mtNano))
	binary.LittleEndian.PutUint64(ext[48:56], dev)
	binary.LittleEndian.PutUint64(ext[56:64], ino)
	if _, err := f.WriteAt(ext[:], sidecarBaseHeaderBytes); err != nil {
		return
	}
	_ = f.Sync()
}

type odConfig struct {
	Dest string
	// CurrentRunID is the current run's ID in the parseArchiveName form
	// ("sfu_<stamp>", via archiveRunID); matching files are excluded from
	// discovery so a retry never dedups against its own previous archive.
	CurrentRunID string
	Buckets      int // MUST match phase 1/2 B
	TempDir      string
	Workers      int  // 0 = min(GOMAXPROCS, archivesNeedingRegen)
	DryRun       bool // -odr: read existing sidecars for dedup, skip all library writes
	Debug        *DebugLog
}

type ODResult struct {
	// sorted (v3) library sidecar paths. dedup gathers each bucket's dest keys
	// from these via one-shot range reads (no per-run routing into scratch).
	DestSidecarPaths []string
	ArchivesTotal    int
	FilesTotal       int
	ArchivesFresh    int
	ArchivesRegen    int
	ArchivesSkipped  int
	// user-visible skipped paths re-emitted post-alt-screen
	SkippedArchivePaths []string
	ArchivesUpgraded    int // parts upgraded v2→v3 in place (one-time per legacy library)
	TotalKeysLoaded     uint64
	Elapsed             time.Duration
}

// runODScan does phase 0 of -od: discover prior archives at the dest, regen
// missing/stale .idx sidecars (decompress), and upgrade legacy v2 sidecars to
// sorted v3 in place (no decompress). It returns the SORTED sidecar paths;
// dedup reads each bucket's library keys from them lazily (no per-run routing).
func runODScan(ctx context.Context, cfg odConfig, m *ODMetrics) (*ODResult, error) {
	if cfg.Buckets <= 0 {
		return nil, fmt.Errorf("odScan: buckets must be > 0")
	}
	if cfg.Dest == "" {
		return nil, fmt.Errorf("odScan: dest is empty")
	}

	started := time.Now()
	if m != nil {
		m.Phase.Store(int32(odPhaseDiscover))
		m.startedAtUnix.Store(started.Unix())
	}

	runs, err := discoverArchiveRuns(cfg.Dest, cfg.CurrentRunID)
	if err != nil {
		return nil, fmt.Errorf("odScan: discover: %w", err)
	}
	cfg.Debug.Event("[od] scan begin: dest=%s, found=%d runs", cfg.Dest, len(runs))

	// orphan sweep, non-fatal. skipped in dry-run: removing stale .idx files
	// from the user's library is a write, and a preview must not mutate it.
	if !cfg.DryRun {
		if n, err := sweepOrphanedSidecars(cfg.Dest, runs, cfg.Debug); err != nil {
			cfg.Debug.Event("[od] orphan sweep: warn: %v", err)
		} else if n > 0 {
			cfg.Debug.Event("[od] orphan sweep: removed %d stale .idx files", n)
		}
		if n, err := sweepOrphanedSearchSidecars(cfg.Dest, runs, cfg.Debug); err != nil {
			cfg.Debug.Event("[od] search orphan sweep: warn: %v", err)
		} else if n > 0 {
			cfg.Debug.Event("[od] search orphan sweep: removed %d stale .sfsidx.json files", n)
		}
	}

	if len(runs) == 0 {
		if m != nil {
			m.Phase.Store(int32(odPhaseDone))
			m.elapsedNanos.Store(int64(time.Since(started)))
		}
		cfg.Debug.Event("[od] scan: no prior archives, skipping phase 0")
		return &ODResult{Elapsed: time.Since(started)}, nil
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
			st, hdr := classifyPartSidecar(p, !cfg.DryRun)
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
		m.ArchivesTotal.Store(int32(len(runs)))
		// runs w/ at least one dirty part, for display continuity
		runsWithDirtyPart := countRunsWithDirtyPart(runs, needRegen)
		m.ArchivesNeedRegen.Store(int32(runsWithDirtyPart))
		m.RegenBytesTotal.Store(regenBytes)
		m.KeysTotalEstimate.Store(keysEstimate)
		m.FilesTotal.Store(int32(totalParts))
		// regen writes a .idx; "parts indexed" progress moves with it
		m.PartsRegenTotal.Store(int32(len(needRegen)))
	}
	cfg.Debug.Event("[od] scan classify: parts_total=%d, parts_need_regen=%d, regen_bytes=%d",
		totalParts, len(needRegen), regenBytes)

	// regen pool, one task per part. corrupt part = skip w/ warning,
	// siblings stay usable
	skippedParts := make(map[string]bool)
	// M-06: parts regen'd into the run temp dir this scan (dry-run only).
	// The library's .idx files are never touched; each temp copy carries the
	// part's real keys and binds its header to the real archive, so the
	// preview counts exactly what the subsequent real -od would load.
	tempSidecars := make(map[string]string)
	if len(needRegen) > 0 {
		if cfg.DryRun {
			// dry-run parity (M-06): dirty parts (missing, stale, malformed
			// sidecar, legacy v2) regenerate into the run temp dir instead of
			// the library. Only parts whose regen actually fails (corrupt
			// archive) stay skipped, matching real -od.
			regenDir := filepath.Join(cfg.TempDir, dryRunRegenDirName)
			if err := os.MkdirAll(regenDir, 0o755); err != nil {
				return nil, fmt.Errorf("odScan: dry-run regen dir: %w", err)
			}
			for i := range needRegen {
				needRegen[i].sidecarOutPath = filepath.Join(regenDir, filepath.Base(needRegen[i].path)+".idx")
			}
		}
		if m != nil {
			m.Phase.Store(int32(odPhaseRegen))
		}
		paths, err := regenParts(ctx, needRegen, cfg, m)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			skippedParts[p] = true
		}
		if m != nil {
			m.ArchivesSkipped.Store(int32(len(paths)))
		}
		if cfg.DryRun {
			for _, p := range needRegen {
				if !skippedParts[p.path] {
					tempSidecars[p.path] = p.sidecarOutPath
				}
			}
		}
	}

	// majority-skipped check: > half the library unreadable = refuse run.
	// silent half-library dedup is worse than failing fast
	totalSkippedParts := len(skippedParts)
	if totalParts > 0 && totalSkippedParts*2 > totalParts {
		return nil, fmt.Errorf(
			"od-scan: %d of %d archive parts in %s were unreadable (%d%%). "+
				"This is too many to safely dedup against -- check the directory and try again",
			totalSkippedParts, totalParts, cfg.Dest,
			(totalSkippedParts*100)/totalParts)
	}

	// run-level counts: fresh = all parts fresh, regen = at least one
	// dirty and all succeeded, skipped = at least one failed. Dry-run
	// regenerates into temp sidecars (M-06), so its runs classify exactly
	// like a real -od run.
	runsFresh, runsRegen, runsSkipped, skippedRunPaths := classifyRunOutcomes(runs, needRegen, skippedParts)

	res := &ODResult{
		ArchivesTotal:   len(runs),
		ArchivesFresh:   runsFresh,
		ArchivesRegen:   runsRegen,
		ArchivesSkipped: runsSkipped,
		// H-21: legacy v2/v3 sidecars no longer upgrade in place — they carry
		// no archive-identity binding, so they regenerate (decompress once).
		// The field stays for -json shape stability and is always 0.
		ArchivesUpgraded:    0,
		SkippedArchivePaths: skippedRunPaths,
		FilesTotal:          totalParts,
		Elapsed:             time.Since(started),
	}

	// dest sidecar list = every non-skipped part's (now sorted v4) sidecar.
	// dedup gathers each bucket's keys from these lazily via one-shot
	// sidecarReader range reads — no per-run routing into scratch buckets.
	//
	// dry-run (M-06): dirty parts were regen'd into temp sidecars under the
	// run temp dir, and those temp paths are used here — same keys, same
	// archive-bound header, library untouched. Only a part whose regen failed
	// (corrupt archive) is omitted, exactly like real -od.
	destSidecars := make([]string, 0, totalParts)
	var totalKeys int64
	for _, r := range runs {
		for _, p := range r.parts {
			if skippedParts[p.path] {
				continue
			}
			if tp := tempSidecars[p.path]; tp != "" {
				hdr, err := readSidecarHeader(tp)
				if err != nil {
					cfg.Debug.Event("[odr] dry-run: temp sidecar for %s unreadable: %v", filepath.Base(p.path), err)
					continue
				}
				destSidecars = append(destSidecars, tp)
				totalKeys += int64(hdr.keyCount)
				continue
			}
			if cfg.DryRun {
				st, hdr := classifyPartSidecar(p, false)
				if st != sidecarStatusFresh {
					// defensive: with dirty parts regen'd into temp sidecars,
					// only a library file changing mid-run can land here
					cfg.Debug.Event("[odr] dry-run: omitting sidecar for %s (status=%s)", filepath.Base(p.path), sidecarStatusName(st))
					continue
				}
				destSidecars = append(destSidecars, p.sidecarPath)
				totalKeys += int64(hdr.keyCount)
				continue
			}
			destSidecars = append(destSidecars, p.sidecarPath)
			if hdr, err := readSidecarHeader(p.sidecarPath); err == nil {
				totalKeys += int64(hdr.keyCount)
			}
		}
	}
	res.DestSidecarPaths = destSidecars
	res.TotalKeysLoaded = uint64(totalKeys)

	if m != nil {
		if totalKeys > 0 {
			m.KeysTotalEstimate.Store(totalKeys)
		}
		// the per-bucket gather happens during dedup; phase-0 work is done.
		m.Phase.Store(int32(odPhaseDone))
		m.elapsedNanos.Store(int64(res.Elapsed))
	}
	cfg.Debug.Event("[od] scan done: archives=%d fresh=%d regen=%d skipped=%d sidecars=%d keys=%d elapsed=%s",
		res.ArchivesTotal, res.ArchivesFresh, res.ArchivesRegen, res.ArchivesSkipped,
		len(destSidecars), totalKeys, res.Elapsed)
	return res, nil
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

// parallel per-part regen. each task streams its archive, hashes every
// parseable credential into its own .idx, then atomically finalises.
// no aggregation, sidecars are self-contained. corrupt parts return in
// the skipped list, siblings stay usable
func regenParts(ctx context.Context, parts []archivePart, cfg odConfig, m *ODMetrics) ([]string, error) {
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

	taskCh := make(chan archivePart)
	errCh := make(chan error, workers)

	var skippedMu sync.Mutex
	var skippedPaths []string

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		workerIdx := i
		go func() {
			defer wg.Done()
			var ws *WorkerStatus
			if m != nil {
				ws = m.workerSlot(workerIdx)
			}
			if ws != nil {
				defer ws.ArchivePath.Store(nil)
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
							filepath.Base(t.path), err)
						select {
						case errCh <- fmt.Errorf("regen %s: %w", filepath.Base(t.path), err):
							cancel()
						default:
						}
						return
					}
					// Don't write to os.Stderr here: phase-0 regen runs while the
					// live TUI owns the screen, so a mid-run warning corrupts the
					// frame. The path is collected below and surfaced cleanly after
					// teardown via renderODSkippedPaths (plus the debug log).
					skippedMu.Lock()
					skippedPaths = append(skippedPaths, t.path)
					skippedMu.Unlock()
					cfg.Debug.Event("[od] regen SKIP (corrupt): part=%s, err=%v",
						filepath.Base(t.path), err)
				} else {
					cfg.Debug.Event("[od] regen done: part=%s, keys=%d, elapsed=%s",
						filepath.Base(t.path), keys, time.Since(started))
				}

				if m != nil {
					m.PartsRegenDone.Add(1)
					if err == nil {
						m.ArchivesRegenedDone.Add(1)
					}
				}
			}
		}()
	}

	go func() {
		defer close(taskCh)
		for _, p := range parts {
			select {
			case taskCh <- p:
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

// newPartSidecarWriter opens a part's .idx writer. sidecarOutPath redirects
// the output (M-06 dry-run regen: the .idx lands in the run temp dir while
// the header still binds to the real archive via archiveSource), so a
// preview can count the part's real keys without touching the library.
func newPartSidecarWriter(t archivePart) (*sidecarWriter, error) {
	if t.sidecarOutPath != "" {
		return newSidecarWriterAtPath(t.sidecarOutPath, t.path)
	}
	return newSidecarWriter(t.path)
}

// streams one part, hashes lines into its temp .idx, atomic finalise.
// errCorruptArchive = skip w/ warning, other errs propagate fatal
func processPartTask(ctx context.Context, t archivePart, decoderConcurrency int, ws *WorkerStatus, fmtr *lineFormatter, m *ODMetrics) (uint64, error) {
	// publish "working on X" BEFORE open so TUI never sees partial state.
	// partIdx/partsTotal kept for TUI continuity, both 1 means rows show
	// just the filename
	if ws != nil {
		name := filepath.Base(t.path)
		ws.ArchivePath.Store(&name)
		ws.PartIdx.Store(1)
		ws.PartsTotal.Store(1)
		ws.BytesDone.Store(0)
		ws.BytesTotal.Store(t.size)
		ws.Committing.Store(false)
	}

	// M-06: sidecarOutPath sends the rebuilt .idx to the run temp dir
	// (dry-run regen) while the header still binds to the real archive.
	sw, err := newPartSidecarWriter(t)
	if err != nil {
		return 0, err
	}

	streamErr := streamArchiveLines(ctx, t.path, decoderConcurrency, ws, func(line string) error {
		// parseStored is the archive reader: FormatRecord inverse + strict,
		// without isLikelyJunk. It matches FormatRecordStable's verification
		// so every written line can be indexed.
		host, _, login, password, ok := parseStored(line)
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

	// the commit window (spill merge + fsync + rename) can outlast the
	// stream by seconds: flag it so the TUI row reads as still working
	// instead of a frozen 100%. cleared on both exit paths so an aborted
	// task never leaves a stale flag in its persistent worker slot.
	if ws != nil {
		ws.Committing.Store(true)
	}
	keys, err := sw.Commit()
	if ws != nil {
		ws.Committing.Store(false)
	}
	if err != nil {
		return 0, fmt.Errorf("sidecar write: %w", err)
	}
	return keys, nil
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
func streamArchiveLines(ctx context.Context, path string, decoderConcurrency int, ws *WorkerStatus, fn func(string) error, m *ODMetrics) error {
	if decoderConcurrency < 1 {
		decoderConcurrency = 1
	}
	f, err := os.Open(path)
	if err != nil {
		// the library lock serializes our own runs, so a live archive should
		// never vanish mid-regen — but a manual/foreign deletion is still
		// possible. report it as a skipped part instead of a fatal error;
		// the sibling parts and the rest of the run stay usable.
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: archive vanished during regen: %v", errCorruptArchive, err)
		}
		return err
	}
	defer f.Close()

	counter := &countingReader{r: f}
	var src io.Reader = counter
	// case-insensitive ext: macOS/Windows surface .ZST/.Zst
	if strings.EqualFold(filepath.Ext(path), ".zst") {
		dec, err := zstd.NewReader(counter,
			zstd.WithDecoderConcurrency(decoderConcurrency),
			zstd.WithDecoderMaxWindow(zstdlimits.MaxDecoderWindow))
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
				m.RegenBytesRead.Add(delta)
			}
			if ws != nil {
				ws.BytesDone.Store(rawNow)
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
