package ulpengine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// each part gets its own .idx in idxSubdirName, no aggregation
func TestRegenPartsMultiPartProducesPerPartSidecars(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260101-000000", 4, 200)

	m := &ODMetrics{}
	_, err := regenParts(context.Background(), parts, odConfig{Workers: 4}, m)
	if err != nil {
		t.Fatal(err)
	}

	if got := m.PartsRegenDone.Load(); got != 4 {
		t.Errorf("partsRegenDone = %d, want 4", got)
	}
	if got := m.ArchivesRegenedDone.Load(); got != 4 {
		t.Errorf("archivesRegenedDone = %d, want 4 (per-part counter)", got)
	}
	for _, p := range parts {
		hdr, err := readSidecarHeader(p.sidecarPath)
		if err != nil {
			t.Fatalf("sidecar read failed for %s: %v", filepath.Base(p.path), err)
		}
		if hdr.keyCount < 200 {
			t.Errorf("part %s keyCount = %d, want >= 200", filepath.Base(p.path), hdr.keyCount)
		}
	}
	// scratch files cleaned up after finalize
	matches, _ := filepath.Glob(filepath.Join(dir, "*.regen-part.*.tmp"))
	if len(matches) != 0 {
		t.Errorf("per-part scratch logs not cleaned up: %v", matches)
	}
}

// clean run: partsRegenDone == input parts exactly. guards vs worker
// silently skipping a part = TUI freezes at 99%
func TestRegenPartsCounterFinalizesEqualsTotal(t *testing.T) {
	dir := t.TempDir()
	const partsPerRun = 3
	const runs = 2
	var allParts []archivePart
	for r := 0; r < runs; r++ {
		runID := fmt.Sprintf("sfu_20260101-00000%d", r)
		ps := buildParts(t, dir, runID, partsPerRun, 100)
		allParts = append(allParts, ps...)
	}

	m := &ODMetrics{}
	m.PartsRegenTotal.Store(int32(len(allParts)))
	_, err := regenParts(context.Background(), allParts, odConfig{Workers: 4}, m)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.PartsRegenDone.Load(); got != int32(len(allParts)) {
		t.Errorf("partsRegenDone = %d, want %d", got, len(allParts))
	}
}

// corrupt part must not poison siblings. main UX gain vs old per-run
// model that skipped a whole run for one bad part
func TestRegenPartsCorruptPartSkipsOnlyItself(t *testing.T) {
	dir := t.TempDir()
	goodParts := buildParts(t, dir, "sfu_20260101-000001_good", 2, 50)

	// non-zstd file w/ .zst ext, decode err = errCorruptArchive
	badPath := filepath.Join(dir, "sfu_20260101-000002_bad_part1.txt.zst")
	if err := os.WriteFile(badPath, []byte("not zstd data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPart := archivePart{path: badPath, partNum: 1, size: 14, sidecarPath: sidecarPathForArchive(badPath)}

	allParts := append([]archivePart(nil), goodParts...)
	allParts = append(allParts, badPart)

	// silence the noisy warning in test output
	origStderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	defer func() { os.Stderr = origStderr; devnull.Close() }()

	m := &ODMetrics{}
	skippedPaths, err := regenParts(context.Background(), allParts, odConfig{Workers: 2}, m)
	if err != nil {
		t.Fatalf("regenParts returned err: %v", err)
	}

	if len(skippedPaths) != 1 || skippedPaths[0] != badPath {
		t.Errorf("skipped paths = %v, want [%s]", skippedPaths, badPath)
	}
	if m.ArchivesRegenedDone.Load() != int32(len(goodParts)) {
		t.Errorf("archivesRegenedDone = %d, want %d", m.ArchivesRegenedDone.Load(), len(goodParts))
	}
	for _, p := range goodParts {
		if _, err := os.Stat(p.sidecarPath); err != nil {
			t.Errorf("good part's sidecar missing: %v", err)
		}
	}
	if _, err := os.Stat(badPart.sidecarPath); err == nil {
		t.Errorf("bad part's sidecar should not have been written")
	}
}

// touch 1 part of multi-part = only that part stale, siblings fresh.
// headline UX win vs old per-run model
func TestPerPartStalenessIsolation(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260201-aaa", 4, 100)

	m := &ODMetrics{}
	if _, err := regenParts(context.Background(), parts, odConfig{Workers: 4}, m); err != nil {
		t.Fatal(err)
	}

	// snapshot modtimes to detect untouched ones
	var stamps []time.Time
	for _, p := range parts {
		fi, err := os.Stat(p.sidecarPath)
		if err != nil {
			t.Fatalf("sidecar missing for %s: %v", filepath.Base(p.path), err)
		}
		stamps = append(stamps, fi.ModTime())
	}

	// touch part 2 to invalidate just its sidecar
	time.Sleep(20 * time.Millisecond) // mtime resolution guard
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(parts[1].path, future, future); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(parts[1].path)
	parts[1].modTime = fi.ModTime()
	for i := range parts {
		fi, _ := os.Stat(parts[i].path)
		parts[i].modTime = fi.ModTime()
	}

	// only part 2 stale
	for i, p := range parts {
		st, _ := classifyPartSidecar(p)
		switch i {
		case 1:
			if st != sidecarStatusStale {
				t.Errorf("part 2 should be stale, got %v", st)
			}
		default:
			if st != sidecarStatusFresh {
				t.Errorf("part %d should be fresh, got %v", i+1, st)
			}
		}
	}
}

// orphan .idx (no matching .zst) gets removed, live ones stay
func TestSweepOrphanedSidecars(t *testing.T) {
	dir := t.TempDir()
	if err := ensureIdxSubdir(dir); err != nil {
		t.Fatal(err)
	}

	// 2 .idx w/ matching archives + 1 orphan
	mkIdx := func(name string) string {
		archive := filepath.Join(dir, name)
		if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		idxPath := sidecarPathForArchive(archive)
		if err := os.WriteFile(idxPath, []byte("dummy idx"), 0o600); err != nil {
			t.Fatal(err)
		}
		return archive
	}
	liveA := mkIdx("sfu_a_part1.txt.zst")
	liveB := mkIdx("sfu_b_part1.txt.zst")

	// orphan: no matching .zst
	orphanPath := filepath.Join(dir, idxSubdirName, "sfu_ghost_part1.txt.zst.idx")
	if err := os.WriteFile(orphanPath, []byte("orphaned"), 0o600); err != nil {
		t.Fatal(err)
	}

	runs, err := discoverArchiveRuns(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	removed, err := sweepOrphanedSidecars(dir, runs)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphan should be gone, got err=%v", err)
	}
	for _, p := range []string{liveA, liveB} {
		if _, err := os.Stat(sidecarPathForArchive(p)); err != nil {
			t.Errorf("live sidecar removed by sweep: %v", err)
		}
	}
}

// first -od run = no subdir yet, sweep must be no-op
func TestSweepOrphanedSidecarsMissingSubdir(t *testing.T) {
	dir := t.TempDir()
	removed, err := sweepOrphanedSidecars(dir, nil)
	if err != nil {
		t.Fatalf("missing subdir should not error: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

// 1 task = decoder consumes all GOMAXPROCS. indirect, just runs to done
func TestRegenPartsSinglePartUsesAllDecoderConcurrency(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260101-000003_single", 1, 50)

	m := &ODMetrics{}
	_, err := regenParts(context.Background(), parts, odConfig{}, m)
	if err != nil {
		t.Fatal(err)
	}
	if m.PartsRegenDone.Load() != 1 {
		t.Errorf("partsRegenDone = %d, want 1", m.PartsRegenDone.Load())
	}
}

// count zstd parts in dir, each w/ lines synthetic creds, sorted by partNum
func buildParts(t *testing.T, dir, runID string, count, lines int) []archivePart {
	t.Helper()
	out := make([]archivePart, count)
	for i := 0; i < count; i++ {
		var raw bytes.Buffer
		for l := 0; l < lines; l++ {
			fmt.Fprintf(&raw, "https://site%d.example.com/login:user%d_%d:password%d\n", i, i, l, l)
		}
		path := filepath.Join(dir, fmt.Sprintf("%s_part%d.txt.zst", runID, i+1))
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w, err := zstd.NewWriter(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(raw.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(path)
		out[i] = archivePart{
			path:        path,
			partNum:     i + 1,
			size:        st.Size(),
			modTime:     st.ModTime(),
			sidecarPath: sidecarPathForArchive(path),
		}
	}
	return out
}

// runODScan returns the sorted library sidecar paths synchronously (no routing
// into scratch). dedup reads each bucket's keys from them lazily.
func TestRunODScanReturnsSidecarPaths(t *testing.T) {
	dir := t.TempDir()
	tempDir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260101-000000_def", 1, 200)
	regenSidecarForTest(t, parts[0].path)

	m := &ODMetrics{}
	res, err := runODScan(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_other",
		Buckets:         4,
		TempDir:         tempDir,
	}, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.ArchivesTotal != 1 {
		t.Errorf("ArchivesTotal = %d, want 1", res.ArchivesTotal)
	}
	if len(res.DestSidecarPaths) != 1 {
		t.Fatalf("DestSidecarPaths = %v, want 1 sidecar", res.DestSidecarPaths)
	}
	if res.TotalKeysLoaded == 0 {
		t.Errorf("TotalKeysLoaded should be > 0")
	}
	// phase-0 work is done synchronously; the gather happens during dedup.
	if got := ODPhase(m.Phase.Load()); got != ODPhaseDone {
		t.Errorf("phase after scan = %v, want odPhaseDone", got)
	}
}

// re-streams part + writes fresh .idx, skips the worker pool
func regenSidecarForTest(t *testing.T, partPath string) {
	t.Helper()
	sw, err := newSidecarWriter(partPath)
	if err != nil {
		t.Fatal(err)
	}
	fmtr := newLineFormatter()
	if err := streamArchiveLines(context.Background(), partPath, 1, nil, func(line string) error {
		host, _, login, password, ok := parseUnion(line)
		if !ok {
			return nil
		}
		return sw.WriteHash(fmtr.HashKey(host, login, password))
	}, nil); err != nil {
		_ = sw.Abort()
		t.Fatal(err)
	}
	if _, err := sw.Commit(); err != nil {
		t.Fatal(err)
	}
}

// race-detector probe: writers mutate slots while reader iterates
func TestWorkerStatusConcurrentReadWrite(t *testing.T) {
	m := &ODMetrics{}
	m.Workers = make([]WorkerStatus, 8)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := range m.Workers {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				name := fmt.Sprintf("part%d_%d", idx, n)
				m.Workers[idx].ArchivePath.Store(&name)
				m.Workers[idx].BytesDone.Add(1)
				n++
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.ActiveWorkers(16)
		}
	}()

	// race them
	for i := 0; i < 1000; i++ {
		_ = m.ActiveWorkers(16)
	}
	close(stop)
	wg.Wait()
}
