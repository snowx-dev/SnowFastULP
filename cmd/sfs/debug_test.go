package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestDebugArtifactPathUnique(t *testing.T) {
	dir := t.TempDir()
	stamp := debugStamp(time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	p1, err := debugArtifactPath(dir, "sfs-debug", ".log", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p1, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := debugArtifactPath(dir, "sfs-debug", ".log", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("expected distinct paths, got %q", p1)
	}
	if filepath.Base(p2) != "sfs-debug-"+stamp+"_2.log" {
		t.Fatalf("path = %q", p2)
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
