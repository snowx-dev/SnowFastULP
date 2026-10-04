package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

func TestCreateArtifactFileUnique(t *testing.T) {
	dir := t.TempDir()
	stamp := debugStamp(time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	f1, p1, err := ulpengine.CreateArtifactFile(dir, "sfs-debug-"+stamp, ".log", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f1.Close()
	f2, p2, err := ulpengine.CreateArtifactFile(dir, "sfs-debug-"+stamp, ".log", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f2.Close()
	if p1 == p2 {
		t.Fatalf("expected distinct paths, got %q", p1)
	}
	if filepath.Base(p2) != "sfs-debug-"+stamp+"_2.log" {
		t.Fatalf("path = %q", p2)
	}
}

// Two sfs processes starting in the same second share the same zero-entropy
// debugStamp; the create must be exclusive so each run gets its own file
// instead of racing O_TRUNC on one prospective path (F11).
func TestConcurrentDebugLogCreation(t *testing.T) {
	dir := t.TempDir()
	stamp := debugStamp(time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	paths := make([]string, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, p, err := ulpengine.CreateArtifactFile(dir, "sfs-debug-"+stamp, ".log", 0o600)
			if err != nil {
				t.Error(err)
				return
			}
			d := newDebugLogFile(f)
			d.Printf("run %d\n", i)
			if err := d.Close(); err != nil {
				t.Error(err)
				return
			}
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: %v", p, err)
			}
			paths[i] = p
		}()
	}
	wg.Wait()

	want := map[string]bool{
		filepath.Join(dir, "sfs-debug-"+stamp+".log"):   true,
		filepath.Join(dir, "sfs-debug-"+stamp+"_2.log"): true,
	}
	for _, p := range paths {
		if p == "" {
			t.Fatal("a run returned no path")
		}
		if !want[p] {
			t.Fatalf("unexpected path %q (want sfs-debug-%s.log or its _2 suffix)", p, stamp)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("missing distinct path; two runs got %v", paths)
	}
}

func TestDebugLogCompletionNilSafe(t *testing.T) {
	var d *debugLog
	d.logCompletion(nil, time.Second, debugRunInfo{})
	d.logTermination(nil, false, time.Second, nil)
}

func TestDebugLogCompletionWritesFooter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "done.log")
	d, err := newDebugLog(path)
	if err != nil {
		t.Fatal(err)
	}

	m := &search.Metrics{}
	m.Phase.Store(search.PhaseDone)
	m.ArchivesTotal.Store(2)
	m.ArchivesIndexed.Store(2)
	m.ArchivesDone.Store(2)
	m.ChunksTotal.Store(2)
	m.ChunksDone.Store(2)

	done := make(chan struct{})
	go func() {
		d.logCompletion(m, time.Minute, debugRunInfo{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("logCompletion deadlocked")
	}

	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "--- Completion ---") {
		t.Fatalf("missing completion footer:\n%s", body)
	}
}

func TestDebugLogProgressRates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "progress.log")
	d, err := newDebugLog(path)
	if err != nil {
		t.Fatal(err)
	}
	m := &search.Metrics{}
	m.IndexBytesTotal.Store(100)
	m.IndexBytesDone.Store(50)
	m.Phase.Store(search.PhaseIndex)
	d.logProgress(m)
	time.Sleep(10 * time.Millisecond)
	m.IndexBytesDone.Store(60)
	d.logProgress(m)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "indexRate=") {
		t.Fatalf("expected rate in log, got:\n%s", body)
	}
}
