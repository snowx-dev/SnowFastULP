package main

// Heavy tier: opt-in via SFL_REALDATA_HEAVY=1 (in addition to SFL_REALDATA_DIR).
// These read multi-GB archives and the 189MB ULP into fresh temp libraries/outputs
// and assert completion + engine counts. The real Library/ is never touched.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

func heavyGate(t *testing.T) {
	t.Helper()
	if os.Getenv(envHeavy) != "1" {
		t.Skipf("set %s=1 (with %s) to run heavy real-data tests", envHeavy, envRealData)
	}
}

// smallestMatch returns the smallest file matching pattern, or skips.
func smallestMatch(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	type sized struct {
		path string
		size int64
	}
	var files []sized
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || fi.IsDir() {
			continue
		}
		files = append(files, sized{m, fi.Size()})
	}
	if len(files) == 0 {
		t.Skipf("no files matching %q", pattern)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].size < files[j].size })
	return files[0].path
}

// runEngineToFile extracts a (potentially huge) input straight to a temp ULP
// file so RAM stays bounded, returning stats, the ULP path, and the debug log.
func runEngineToFile(t *testing.T, input string, passwords []string) (sflog.ExtractStats, string, string) {
	t.Helper()
	ulpPath := filepath.Join(t.TempDir(), "heavy_ulp.txt")
	f, err := os.Create(ulpPath)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var dbg strings.Builder
	eng := &sflog.Engine{
		Workers:   runtime.GOMAXPROCS(0),
		Passwords: passwords,
		Debug: func(format string, args ...any) {
			mu.Lock()
			fmt.Fprintf(&dbg, format+"\n", args...)
			mu.Unlock()
		},
	}
	stats, _, runErr := eng.Run(context.Background(), input, f)
	closeErr := f.Close()
	if runErr != nil {
		t.Fatalf("engine run on %s: %v", input, runErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return stats, ulpPath, dbg.String()
}

// TestRealDataHeavyZipExtractAndIngest extracts the smallest top-level .zip and,
// if it yields credentials, ingests them into a fresh library.
func TestRealDataHeavyZipExtractAndIngest(t *testing.T) {
	root := realDataRoot(t)
	heavyGate(t)
	zip := smallestMatch(t, filepath.Join(root, "fullz", "*.zip"))
	t.Logf("heavy zip: %s", zip)

	stats, ulp, dbg := runEngineToFile(t, zip, []string{""})
	if stats.ArchivesScanned < 1 {
		t.Fatalf("ArchivesScanned = %d, want >=1", stats.ArchivesScanned)
	}
	t.Logf("emitted=%d seen=%d files=%d skipped_archives=%d pwd_not_found=%d",
		stats.Emitted, stats.Credentials, stats.FilesScanned, stats.SkippedArchives, stats.PasswordNotFound)
	if !strings.Contains(dbg, "archive ") {
		t.Fatalf("expected per-archive debug events:\n%s", tail(dbg, 2000))
	}

	if stats.PasswordNotFound > 0 {
		t.Skipf("archive (or a nested member) is encrypted with no matching password; extraction completed, skipping ingest")
	}
	// With recursive nested-archive handling a zip-of-zips now yields its inner
	// credentials, so a non-encrypted archive must emit something.
	if stats.Emitted == 0 {
		t.Fatalf("non-encrypted archive yielded no ULP; nested recursion expected credentials (stats = %+v)", stats)
	}

	lib := t.TempDir()
	m := &ulpengine.Metrics{}
	if _, err := ulpengine.Ingest(context.Background(), ulpengine.IngestOptions{
		ULPPath: ulp, LibraryDir: lib, Workers: runtime.GOMAXPROCS(0),
	}, m); err != nil {
		t.Fatalf("ingest heavy ULP: %v", err)
	}
	if m.LinesUnique.Load() == 0 {
		t.Fatal("heavy ingest added 0 unique lines")
	}
}

// TestRealDataHeavyRarExtract extracts the smallest top-level .rar to prove the
// rar path handles multi-GB inputs without exhausting memory.
func TestRealDataHeavyRarExtract(t *testing.T) {
	root := realDataRoot(t)
	heavyGate(t)
	rar := smallestMatch(t, filepath.Join(root, "fullz", "*.rar"))
	t.Logf("heavy rar: %s", rar)

	stats, _, dbg := runEngineToFile(t, rar, []string{""})
	if stats.ArchivesScanned < 1 {
		t.Fatalf("ArchivesScanned = %d, want >=1", stats.ArchivesScanned)
	}
	t.Logf("emitted=%d seen=%d files=%d skipped_archives=%d pwd_not_found=%d",
		stats.Emitted, stats.Credentials, stats.FilesScanned, stats.SkippedArchives, stats.PasswordNotFound)
	if !strings.Contains(dbg, "archive ") {
		t.Fatalf("expected per-archive debug events:\n%s", tail(dbg, 2000))
	}
}

// TestRealDataHeavyULPIngest ingests the 189MB raw ULP into a fresh library.
func TestRealDataHeavyULPIngest(t *testing.T) {
	root := realDataRoot(t)
	heavyGate(t)
	ulp := requireFile(t, filepath.Join(root, "raws", "txt",
		"@FORZATRAFFICx_EXTREME_MIX_PACK_41,100_BONUS_UPDATE_—_JUNE_2026.txt"))

	lib := t.TempDir()
	m := &ulpengine.Metrics{}
	dbgPath := filepath.Join(t.TempDir(), "heavy_ingest.log")
	elog, err := ulpengine.NewDebugLog(dbgPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ulpengine.Ingest(context.Background(), ulpengine.IngestOptions{
		ULPPath: ulp, LibraryDir: lib, Workers: runtime.GOMAXPROCS(0), Debug: elog,
	}, m)
	_ = elog.Close()
	if err != nil {
		t.Fatalf("heavy ULP ingest: %v", err)
	}
	if m.LinesUnique.Load() == 0 {
		t.Fatal("heavy ULP ingest added 0 unique lines")
	}
	t.Logf("unique=%d skipped_by_dest=%d", m.LinesUnique.Load(), m.LinesSkippedByDest.Load())
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
