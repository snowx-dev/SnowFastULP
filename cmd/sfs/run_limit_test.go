package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// -l N stops after N total hits and exits cleanly, emitting exactly N lines.
func TestRunGlobalHitLimit(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")

	var buf bytes.Buffer
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&buf, "example.com:user%02d@example.com:needle\n", i)
	}
	writeZST(t, arch, buf.Bytes())

	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	const limit = 10
	outPath := filepath.Join(dir, "hits.txt")
	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{arch},
		workers:  2,
		outFile:  outPath,
		stream:   true,
		limit:    limit,
		started:  time.Now(),
		metrics:  metrics,
	})
	if err != nil {
		t.Fatalf("run returned error on limit stop: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	got := bytes.Count(data, []byte("\n"))
	if got != limit {
		t.Fatalf("emitted %d lines, want %d", got, limit)
	}
	if metrics.Hits.Load() != int64(limit) {
		t.Fatalf("metrics.Hits = %d, want %d", metrics.Hits.Load(), limit)
	}
}

// limit larger than the match count still returns every hit, no early stop.
func TestRunGlobalHitLimitAboveTotal(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")

	var buf bytes.Buffer
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&buf, "example.com:user%d@example.com:needle\n", i)
	}
	writeZST(t, arch, buf.Bytes())

	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "hits.txt")
	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{arch},
		workers:  2,
		outFile:  outPath,
		stream:   true,
		limit:    1000,
		started:  time.Now(),
		metrics:  metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Hits.Load() != 5 {
		t.Fatalf("metrics.Hits = %d, want 5", metrics.Hits.Load())
	}
}
