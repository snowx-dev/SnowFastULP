package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"

	"github.com/klauspost/compress/zstd"
)

func writeZST(t *testing.T, path string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunWritesHitsToOutputFile(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	line := "example.com:user@example.com:needle\n"
	writeZST(t, arch, []byte(line))

	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "hits.txt")
	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{arch},
		workers:  1,
		outFile:  outPath,
		stream:   false, // stats/file-only: ordered archive output
		started:  time.Now(),
		metrics:  metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty output file")
	}
	if !strings.Contains(string(data), "needle") {
		t.Fatalf("output %q missing needle", data)
	}
	if metrics.Hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", metrics.Hits.Load())
	}
}

func TestRunStreamsHitsToStdout(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	writeZST(t, arch, []byte("example.com:user@example.com:needle\n"))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	metrics := &search.Metrics{}
	if err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{arch},
		workers:  1,
		stream:   true,
		started:  time.Now(),
		metrics:  metrics,
		stdout:   &stdout,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "needle") {
		t.Fatalf("stream mode stdout missing needle: %q", stdout.String())
	}
	if metrics.Hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", metrics.Hits.Load())
	}
}

func TestRunTeesHitsToStdoutAndFile(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	writeZST(t, arch, []byte("example.com:user@example.com:needle\n"))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "hits.txt")
	var stdout bytes.Buffer
	metrics := &search.Metrics{}
	if err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{arch},
		workers:  1,
		outFile:  outPath,
		stream:   true, // -o without -stats: tee
		started:  time.Now(),
		metrics:  metrics,
		stdout:   &stdout,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "needle") {
		t.Fatalf("tee mode stdout missing needle: %q", stdout.String())
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "needle") {
		t.Fatalf("tee mode file missing needle: %q", data)
	}
	if metrics.Hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", metrics.Hits.Load())
	}
}
