package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two concurrent runs sharing one log dir must never clobber each other's
// debug log: same-second starts used to collide on
// sfl_debug_<YYYYMMDD_HHMMSS>.log and only one file survived. The name is now
// stamped with the per-run RunStamp plus a CreateTemp random suffix, so two
// newDebugLogger calls with identical config still produce two distinct logs.
func TestDebugLogNamesUniqueAcrossConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	cfg := runConfig{Debug: true, LibraryDir: dir, Started: started, RunStamp: "20260924_ab12cd"}

	d1 := newDebugLogger(cfg)
	if d1 == nil {
		t.Fatal("first debug logger not created")
	}
	d2 := newDebugLogger(cfg)
	if d2 == nil {
		t.Fatal("second debug logger not created")
	}

	matches, err := filepath.Glob(filepath.Join(dir, "sfl_debug_*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d debug logs (%v), want 2 distinct files for two same-second runs", len(matches), matches)
	}
	if matches[0] == matches[1] {
		t.Fatalf("both runs wrote %s; names must be unique", matches[0])
	}
}

// The ingest engine debug log used to be a bare
// sfl_ingest_debug_<stamp>.log opened O_TRUNC with no uniqueness at all: two
// same-second -od runs clobbered each other's file. It now goes through the
// exclusive-create allocator, so concurrent callers get the _2 suffix on
// collision instead of a shared path.
func TestIngestDebugLogUniqueAcrossConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	cfg := runConfig{Debug: true, LibraryDir: dir, Started: started}

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			elog := newIngestDebugLog(cfg)
			if elog == nil {
				t.Error("ingest debug log not created")
				return
			}
			_ = elog.Close()
		}()
	}
	wg.Wait()

	matches, err := filepath.Glob(filepath.Join(dir, "sfl_ingest_debug_*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d ingest debug logs (%v), want 2 distinct files for concurrent same-second runs", len(matches), matches)
	}
	if matches[0] == matches[1] {
		t.Fatalf("both runs wrote %s; names must be unique", matches[0])
	}
}

// The name carries the run stamp so a log is attributable to its run even
// without opening it; a caller that skips ensureRunStamp still gets a unique,
// timestamped name via the CreateTemp random suffix.
func TestDebugLogNameCarriesRunStamp(t *testing.T) {
	dir := t.TempDir()
	cfg := runConfig{Debug: true, LibraryDir: dir, Started: time.Now(), RunStamp: "20260924_zz99zz"}
	d := newDebugLogger(cfg)
	if d == nil {
		t.Fatal("debug logger not created")
	}
	name := d.f.Name()
	if !strings.Contains(filepath.Base(name), "20260924_zz99zz") {
		t.Fatalf("debug log %q does not carry the run stamp", name)
	}

	// Empty-stamp fallback: still created, still under the sfl_debug_ prefix.
	d2 := newDebugLogger(runConfig{Debug: true, LibraryDir: dir, Started: time.Now()})
	if d2 == nil {
		t.Fatal("fallback debug logger not created")
	}
	if base := filepath.Base(d2.f.Name()); !strings.HasPrefix(base, "sfl_debug_") {
		t.Fatalf("fallback debug log %q lost the sfl_debug_ prefix", base)
	}
	// Close both so the temp files do not linger open for the test process.
	defer d2.f.Close()
	defer d.f.Close()
	_ = os.Remove(name)
}
