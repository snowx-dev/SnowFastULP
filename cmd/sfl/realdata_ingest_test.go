package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// intAfter returns the integer immediately following key in s.
func intAfter(s, key string) (int, bool) {
	i := strings.Index(s, key)
	if i < 0 {
		return 0, false
	}
	j := i + len(key)
	k := j
	for k < len(s) && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	if k == j {
		return 0, false
	}
	n, err := strconv.Atoi(s[j:k])
	return n, err == nil
}

// ingestRealULP runs an in-process ingest with an engine debug log captured to a
// temp file, returning the resolved run, metrics, and the debug transcript.
func ingestRealULP(t *testing.T, ulpPath, libDir string, m *ulpengine.Metrics, onResolved func(*ulpengine.Resolved)) (*ulpengine.Resolved, string) {
	t.Helper()
	dbgPath := filepath.Join(t.TempDir(), "ingest_debug.log")
	elog, err := ulpengine.NewDebugLog(dbgPath)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ulpengine.Ingest(context.Background(), ulpengine.IngestOptions{
		ULPPath: ulpPath, LibraryDir: libDir, Workers: 2,
		Debug: elog, OnResolved: onResolved,
	}, m)
	_ = elog.Close()
	if err != nil {
		t.Fatalf("ingest %s -> %s: %v", ulpPath, libDir, err)
	}
	return r, readFileString(t, dbgPath)
}

// TestRealDataIngestEmptyWarmCold ingests the same raw ULP three ways and
// asserts the engine debug markers prove the right phase-0 behaviour each time.
func TestRealDataIngestEmptyWarmCold(t *testing.T) {
	root := realDataRoot(t)
	ulp := smallULP(t, root)
	lib := t.TempDir()

	// EMPTY: no prior archives, phase 0 skipped entirely.
	m1 := &ulpengine.Metrics{}
	_, dbg1 := ingestRealULP(t, ulp, lib, m1, nil)
	unique := m1.LinesUnique.Load()
	if unique == 0 {
		t.Fatal("empty ingest added 0 unique lines")
	}
	if !strings.Contains(dbg1, "no prior archives") {
		t.Fatalf("empty lib should skip phase 0; debug:\n%s", dbg1)
	}
	if got := libUniqueCount(t, lib); got != unique {
		t.Fatalf("library unique = %d, want %d", got, unique)
	}

	// WARM: prior archive's sidecar is intact, nothing needs regen, and the
	// identical ULP is fully absorbed by the dest.
	m2 := &ulpengine.Metrics{}
	_, dbg2 := ingestRealULP(t, ulp, lib, m2, nil)
	if got := m2.LinesUnique.Load(); got != 0 {
		t.Fatalf("warm re-ingest added %d new lines, want 0", got)
	}
	// LinesSkippedByDest counts raw input occurrences matched in the dest, so it
	// is >= the distinct count (the ULP carries intra-file duplicates).
	if got := m2.LinesSkippedByDest.Load(); got < unique {
		t.Fatalf("warm re-ingest skipped %d, want >= %d already-in-library", got, unique)
	}
	if n, ok := intAfter(dbg2, "parts_need_regen="); !ok || n != 0 {
		t.Fatalf("warm lib should need no regen; parts_need_regen=%d ok=%v\n%s", n, ok, dbg2)
	}

	// COLD: drop the sidecar index so phase 0 must regenerate it.
	if err := os.RemoveAll(filepath.Join(lib, "sfu_dedup_idx")); err != nil {
		t.Fatal(err)
	}
	m3 := &ulpengine.Metrics{}
	var od3 *ulpengine.ODMetrics
	_, dbg3 := ingestRealULP(t, ulp, lib, m3, func(r *ulpengine.Resolved) { od3 = r.OdMetrics })
	if n, ok := intAfter(dbg3, "parts_need_regen="); !ok || n < 1 {
		t.Fatalf("cold lib should regen >=1 part; parts_need_regen=%d ok=%v\n%s", n, ok, dbg3)
	}
	if od3 == nil || od3.RegenBytesTotal.Load() == 0 {
		t.Fatal("cold ingest reported no regen bytes")
	}
}

// TestRealDataParitySflODvsIngest proves the sfl -od in-process path produces a
// library byte-identical (by normalized content) to feeding the same extracted
// ULP through ulpengine.Ingest directly.
func TestRealDataParitySflODvsIngest(t *testing.T) {
	root := realDataRoot(t)
	victim := firstVictimFolder(t, root)

	libOD := t.TempDir()
	if err := run(runConfig{
		Input: victim, LibraryDir: libOD,
		Workers: 2, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
	}); err != nil {
		t.Fatalf("sfl -od: %v", err)
	}

	outDir := t.TempDir()
	if err := run(runConfig{
		Input: victim, OutputDir: outDir,
		Workers: 2, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
	}); err != nil {
		t.Fatalf("sfl -o: %v", err)
	}
	ulp := globOne(t, filepath.Join(outDir, "sfl_*.txt"))

	libIngest := t.TempDir()
	if _, err := ulpengine.Ingest(context.Background(), ulpengine.IngestOptions{
		ULPPath: ulp, LibraryDir: libIngest, Workers: 2,
	}, &ulpengine.Metrics{}); err != nil {
		t.Fatalf("ulpengine.Ingest: %v", err)
	}

	if a, b := normLibHash(t, libOD), normLibHash(t, libIngest); a != b {
		t.Fatalf("library parity mismatch:\n  sfl -od  = %s\n  ingest   = %s", a, b)
	}
}

// TestRealDataColdRegenMonotonic drives a real cold-library regen ingest and
// polls the exact TUI progress function from a separate goroutine, asserting the
// user-visible bar is non-decreasing, bounded, ends at 1.0, and that regen was
// actually exercised.
func TestRealDataColdRegenMonotonic(t *testing.T) {
	root := realDataRoot(t)
	ulp := smallULP(t, root)
	lib := t.TempDir()

	// Seed a one-archive library, then force it cold.
	if _, err := ulpengine.Ingest(context.Background(), ulpengine.IngestOptions{
		ULPPath: ulp, LibraryDir: lib, Workers: 2,
	}, &ulpengine.Metrics{}); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(lib, "sfu_dedup_idx")); err != nil {
		t.Fatal(err)
	}

	ulpBytes := fileSizeOrZero(ulp)
	m := &ulpengine.Metrics{}
	var od atomic.Pointer[ulpengine.ODMetrics]
	dbgPath := filepath.Join(t.TempDir(), "regen.log")
	elog, err := ulpengine.NewDebugLog(dbgPath)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, e := ulpengine.Ingest(context.Background(), ulpengine.IngestOptions{
			ULPPath: ulp, LibraryDir: lib, Workers: 2, Debug: elog,
			OnResolved: func(r *ulpengine.Resolved) { od.Store(r.OdMetrics) },
		}, m)
		done <- e
	}()

	var last float64
	var samples []float64
	sample := func() { samples = append(samples, monotonic(firstFrac(m, od.Load(), ulpBytes), &last)) }

poll:
	for {
		select {
		case e := <-done:
			if e != nil {
				_ = elog.Close()
				t.Fatalf("cold ingest: %v", e)
			}
			break poll
		case <-time.After(15 * time.Millisecond):
			sample()
		}
	}
	sample() // final settle: phase is Done -> 1.0
	_ = elog.Close()

	for i, f := range samples {
		if f < 0 || f > 1 {
			t.Fatalf("sample %d out of range: %.4f", i, f)
		}
		if i > 0 && f < samples[i-1] {
			t.Fatalf("user-visible bar reversed: %.4f -> %.4f", samples[i-1], f)
		}
	}
	if got := samples[len(samples)-1]; got != 1.0 {
		t.Fatalf("final sample = %.4f, want 1.0", got)
	}
	dbg := readFileString(t, dbgPath)
	if n, ok := intAfter(dbg, "parts_need_regen="); !ok || n < 1 {
		t.Fatalf("expected regen to be exercised; parts_need_regen=%d ok=%v\n%s", n, ok, dbg)
	}
}

// firstFrac is the pre-dedup fraction of the live TUI mapping, used by the
// polling loop above.
func firstFrac(m *ulpengine.Metrics, od *ulpengine.ODMetrics, ulpBytes int64) float64 {
	f, _ := ingestProgress(m, od, ulpBytes)
	return f
}

// TestRealDataLibrarySize asserts the size reported on the -od summary matches
// the actual distinct line count of the resulting library.
func TestRealDataLibrarySize(t *testing.T) {
	root := realDataRoot(t)
	ulp := smallULP(t, root)
	lib := t.TempDir()

	m := &ulpengine.Metrics{}
	r, _ := ingestRealULP(t, ulp, lib, m, nil)
	if got, want := ingestLibraryLines(r, m), libUniqueCount(t, lib); got != want {
		t.Fatalf("ingestLibraryLines = %d, library distinct lines = %d", got, want)
	}
}
