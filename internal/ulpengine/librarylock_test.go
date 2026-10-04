package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLibraryLockExclusiveBlocksUntilRelease: a second exclusive acquire must
// retry (not crash, not steal the lock) while the first holds it, and must
// succeed promptly after release.
func TestLibraryLockExclusiveBlocksUntilRelease(t *testing.T) {
	dir := t.TempDir()

	first, err := acquireLibraryLock(context.Background(), dir, false, true, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := acquireLibraryLock(ctx, dir, false, true, nil); err == nil {
		t.Fatal("second exclusive acquire succeeded while lock held; want blocked")
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		first.release()
	}()

	start := time.Now()
	second, err := acquireLibraryLock(context.Background(), dir, false, true, nil)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("acquired after only %s; expected to wait for the held lock", elapsed)
	}
	second.release()

	// double release must be a no-op
	first.release()
}

// TestLibraryLockContextCancel: cancellation while blocked must surface the
// context error, not spin forever.
func TestLibraryLockContextCancel(t *testing.T) {
	dir := t.TempDir()
	holder, err := acquireLibraryLock(context.Background(), dir, false, true, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer holder.release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = acquireLibraryLock(ctx, dir, false, true, nil)
	if err == nil {
		t.Fatal("canceled acquire succeeded; want context error")
	}
}

// TestLibraryLockSharedOverlaps: two shared (dry-run) acquisitions must both
// succeed while an exclusive holder would block them.
func TestLibraryLockSharedOverlaps(t *testing.T) {
	dir := t.TempDir()
	a, err := acquireLibraryLock(context.Background(), dir, true, true, nil)
	if err != nil {
		t.Fatalf("shared acquire A: %v", err)
	}
	defer a.release()

	done := make(chan *libraryLock, 1)
	errCh := make(chan error, 1)
	go func() {
		b, err := acquireLibraryLock(context.Background(), dir, true, true, nil)
		if err != nil {
			errCh <- err
			return
		}
		done <- b
	}()
	select {
	case b := <-done:
		b.release()
	case err := <-errCh:
		t.Fatalf("second shared acquire failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("second shared acquire blocked; shared locks must overlap")
	}
}

// TestLibraryLockDryRunSkipsCreation: with canCreate=false and no lock file
// present, a dry-run acquire must succeed without creating any file — a
// preview must not mutate the library.
func TestLibraryLockDryRunSkipsCreation(t *testing.T) {
	dir := t.TempDir()
	lk, err := acquireLibraryLock(context.Background(), dir, true, false, nil)
	if err != nil {
		t.Fatalf("dry-run acquire: %v", err)
	}
	if lk != nil {
		lk.release()
	}
	if _, err := os.Stat(filepath.Join(dir, libraryLockFileName)); !os.IsNotExist(err) {
		t.Fatalf("lock file created during dry-run: %v", err)
	}
}

// TestLibraryLockDryRunWaitsForRealRun: a dry-run shared acquire against an
// existing lock file must block while a real run holds it exclusively, and
// must proceed once released.
func TestLibraryLockDryRunWaitsForRealRun(t *testing.T) {
	dir := t.TempDir()
	holder, err := acquireLibraryLock(context.Background(), dir, false, true, nil)
	if err != nil {
		t.Fatalf("exclusive acquire: %v", err)
	}
	// lock file now exists (real-run semantics)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := acquireLibraryLock(ctx, dir, true, false, nil); err == nil {
		t.Fatal("dry-run shared acquire succeeded against exclusive holder; want blocked")
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		holder.release()
	}()
	lk, err := acquireLibraryLock(context.Background(), dir, true, false, nil)
	if err != nil {
		t.Fatalf("dry-run acquire after release: %v", err)
	}
	lk.release()
}

// TestODConcurrentIngestRunsSerialize: two full ingest runs (-od) against the
// same library directory overlap in time. The library lock must serialize
// their phase-0-through-commit spans so neither deletes the other's sidecars,
// observes a partial archive, or loses records. A's 5 creds + B's (2 shared +
// 3 new) must total 8 unique library records regardless of run order, and the
// two skipped-by-dest counts must sum to 2 (the second runner saw the first's
// committed sidecar).
func TestODConcurrentIngestRunsSerialize(t *testing.T) {
	libDir := t.TempDir()
	stage := t.TempDir()

	credsA := []string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
		"https://c.example.com:user3:pw3",
		"https://d.example.com:user4:pw4",
		"https://e.example.com:user5:pw5",
	}
	credsB := []string{
		"https://a.example.com:user1:pw1", // shared with A
		"https://b.example.com:user2:pw2", // shared with A
		"https://f.example.com:user6:pw6", // B-only
		"https://g.example.com:user7:pw7", // B-only
		"https://h.example.com:user8:pw8", // B-only
	}

	runAsync := func(input []string, stamp string) (*Metrics, chan error) {
		in := filepath.Join(t.TempDir(), "in_"+stamp+".txt")
		writeFileContent(t, in, joinLines(input))
		logPath := filepath.Join(stage, "dbg_"+stamp+".log")
		dbg, _ := NewDebugLog(logPath)
		r, err := Resolve(Config{
			Inputs:       []string{in},
			Output:       filepath.Join(libDir, "sfu_"+stamp+".txt.zst"),
			TempDir:      filepath.Join(stage, stamp),
			FastPathOff:  true,
			Buckets:      4,
			Compress:     true,
			DestDedup:    true,
			DestDedupDir: libDir,
			RunStamp:     stamp,
			Debug:        dbg,
		})
		if err != nil {
			t.Fatalf("resolve %s: %v", stamp, err)
		}
		m := &Metrics{}
		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(context.Background(), &Resolved{
				Cfg:          r.Cfg,
				TotalInputs:  r.TotalInputs,
				mem:          r.mem,
				BucketCount:  4,
				Workers:      1,
				DedupWorkers: 1,
				chunkBytes:   1 << 20,
				TempDir:      filepath.Join(stage, stamp),
			}, m)
		}()
		return m, errCh
	}

	mA, errA := runAsync(credsA, "20260920_aaaa")
	mB, errB := runAsync(credsB, "20260920_bbbb")

	if err := <-errA; err != nil {
		t.Fatalf("run A: %v", err)
	}
	if err := <-errB; err != nil {
		t.Fatalf("run B: %v", err)
	}

	// both runs committed; neither observed a partial archive or lost records
	totalLines := mB.LinesUnique.Load() + mA.LinesUnique.Load()
	if totalLines != 8 {
		t.Errorf("total unique lines across runs = %d, want 8", totalLines)
	}
	if skips := mA.LinesSkippedByDest.Load() + mB.LinesSkippedByDest.Load(); skips != 2 {
		t.Errorf("sum linesSkippedByDest = %d, want 2 (second run saw first run's committed sidecar)", skips)
	}

	// every sidecar of every archive is present and readable (sweeps ran
	// serialized, so nothing was deleted out from under a live archive)
	matches, err := filepath.Glob(filepath.Join(libDir, "sfu_*.txt.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("archives = %v, want 2", matches)
	}
	for _, arch := range matches {
		if _, err := readSidecarHeader(sidecarPathForArchive(arch)); err != nil {
			t.Errorf("sidecar for %s unreadable after concurrent runs: %v", filepath.Base(arch), err)
		}
	}
	// search sidecars committed by both runs are intact too
	searchMatches, err := filepath.Glob(filepath.Join(libDir, "sfu_search_idx", "*.sfsidx.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(searchMatches) != 2 {
		t.Errorf("search sidecars = %v, want 2", searchMatches)
	}

	// success must leave no committed artifact cleanup-registered
	for _, p := range SnapshotCleanupPaths() {
		if strings.HasPrefix(p, libDir) {
			t.Errorf("committed artifact still cleanup-registered after success: %s", p)
		}
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
