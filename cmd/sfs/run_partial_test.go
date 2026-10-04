package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// TestRunPartialScanEmitsHitsThenFails: mixing one valid and one corrupt
// archive must still emit the valid hits, then fail with a PartialScanError
// naming the corrupt input — one bad archive never hides valid hits, and the
// run must not look COMPLETE.
func TestRunPartialScanEmitsHitsThenFails(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.zst")
	writeZST(t, valid, []byte("example.com:user@example.com:needle\n"))
	if _, err := index.Build(context.Background(), valid, nil, nil); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt.zst")
	if err := os.WriteFile(corrupt, []byte("definitely not zstd"), 0o644); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "hits.txt")
	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{valid, corrupt},
		workers:  2,
		outFile:  outPath,
		stream:   false,
		started:  time.Now(),
		metrics:  metrics,
	})
	if err == nil {
		t.Fatal("run swallowed a corrupt archive: want failure")
	}
	var perr *search.PartialScanError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *search.PartialScanError", err, err)
	}
	if !strings.Contains(perr.Error(), "corrupt.zst") {
		t.Fatalf("error does not name the failed input: %v", perr)
	}
	data, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), "needle") {
		t.Fatalf("valid hits not emitted despite the partial failure: %q", data)
	}
	if metrics.Hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", metrics.Hits.Load())
	}
}

// TestRunTxtPartialScanEmitsHitsThenFails: the same contract in -txt mode with
// an unreadable file.
func TestRunTxtPartialScanEmitsHitsThenFails(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	if err := os.WriteFile(good, []byte("a needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.txt")

	var stdout bytes.Buffer
	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		txtMode:  true,
		archives: []string{good, missing},
		workers:  2,
		stream:   true,
		started:  time.Now(),
		metrics:  metrics,
		stdout:   &stdout,
	})
	if err == nil {
		t.Fatal("run swallowed an unreadable txt file: want failure")
	}
	var perr *search.PartialScanError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *search.PartialScanError", err, err)
	}
	if !strings.Contains(perr.Error(), "missing.txt") {
		t.Fatalf("error does not name the failed input: %v", perr)
	}
	if !strings.Contains(stdout.String(), "needle") {
		t.Fatalf("valid hits not emitted despite the partial failure: %q", stdout.String())
	}
}

// TestRunHitCapOnlyTruncationStillClean: the documented -l hit cap is a
// deliberate, user-requested truncation — not an error. A run that stops only
// because of the cap must return nil (exit 0 / COMPLETE).
func TestRunHitCapOnlyTruncationStillClean(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	writeZST(t, arch, []byte("example.com:needle\nexample.com:needle\nexample.com:needle\n"))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{arch},
		workers:  1,
		limit:    2,
		stream:   true,
		started:  time.Now(),
		metrics:  metrics,
		stdout:   &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("hit-cap-only truncation must exit clean, got: %v", err)
	}
	if metrics.Hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 (the cap)", metrics.Hits.Load())
	}
}

// TestRenderIncompleteSummary: the terminal summary line must say INCOMPLETE,
// never COMPLETE, when the scan was partial.
func TestRenderIncompleteSummary(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(3)
	out := strings.Join(renderIncompleteSummary(time.Now(), m, "", "", nil), "\n")
	if !strings.Contains(out, "INCOMPLETE") {
		t.Fatalf("missing INCOMPLETE terminal line:\n%s", out)
	}
	if strings.Contains(collapseRenderedText(out), "✓ COMPLETE") {
		t.Fatalf("incomplete summary must never show COMPLETE:\n%s", out)
	}
}
