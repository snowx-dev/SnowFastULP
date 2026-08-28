package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// multiPatternRunConfig builds a runConfig for a multi-pattern (-f) run over
// plain .txt files, mirroring how main() wires a real -f invocation. stream
// true → all hits to stdout (interleaved); with multiSink → one file per
// pattern (file-only).
func multiPatternRunConfig(t *testing.T, dir string, files []string, patterns []string, stdout io.Writer, multiSink *dispatchSink) runConfig {
	t.Helper()
	patBytes := make([][]byte, len(patterns))
	for i, p := range patterns {
		patBytes[i] = []byte(p)
	}
	matcher := search.NewMultiMatcher(patBytes)
	return runConfig{
		root:      dir,
		archives:  files,
		txtMode:   true,
		workers:   1,
		stream:    multiSink == nil,
		started:   time.Now(),
		metrics:   &search.Metrics{},
		stdout:    stdout,
		matcher:   matcher,
		multiSink: multiSink,
	}
}

func TestRunMultiPatternStreamsToStdout(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha foo beta\nbar gamma\nfoo again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cfg := multiPatternRunConfig(t, dir, []string{f}, []string{"foo", "bar"}, &stdout, nil)
	if err := run(context.Background(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	sort.Strings(got)
	// "foo" matches 2 lines, "bar" matches 1 line → 3 hits interleaved.
	want := []string{"alpha foo beta", "bar gamma", "foo again"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got %d lines %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q want %q", i, got[i], want[i])
		}
	}
	if cfg.metrics.Hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", cfg.metrics.Hits.Load())
	}
}

func TestRunMultiPatternWritesPerPatternFiles(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha foo beta\nbar gamma\nfoo again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	if _, err := validateFileOutputDir(outDir); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	patterns := []string{"foo", "bar"}
	files, paths, err := allocatePatternFiles(outDir, patterns, stamp)
	if err != nil {
		t.Fatal(err)
	}
	sink := newDispatchSink(files, false)
	cfg := multiPatternRunConfig(t, dir, []string{f}, patterns, nil, sink)
	cfg.started = stamp
	if err := run(context.Background(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.files[0] != nil || sink.files[1] != nil {
		t.Fatal("run should close the sink on return")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if cfg.metrics.Hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", cfg.metrics.Hits.Load())
	}
	// Two files, one per pattern.
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want 2", paths)
	}
	fooBytes, _ := os.ReadFile(paths[0])
	barBytes, _ := os.ReadFile(paths[1])
	// Pattern index 0 = "foo" → 2 lines; index 1 = "bar" → 1 line.
	fooCount := strings.Count(string(fooBytes), "\n")
	barCount := strings.Count(string(barBytes), "\n")
	if fooCount != 2 {
		t.Fatalf("foo file = %q, want 2 lines", fooBytes)
	}
	if barCount != 1 {
		t.Fatalf("bar file = %q, want 1 line", barBytes)
	}
	if !strings.Contains(string(fooBytes), "alpha foo beta") || !strings.Contains(string(fooBytes), "foo again") {
		t.Fatalf("foo file missing expected lines: %q", fooBytes)
	}
	if !strings.Contains(string(barBytes), "bar gamma") {
		t.Fatalf("bar file missing expected line: %q", barBytes)
	}
}

func TestRunMultiPatternLimitGlobal(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("foo\nfoo\nbar\nbar\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cfg := multiPatternRunConfig(t, dir, []string{f}, []string{"foo", "bar"}, &stdout, nil)
	cfg.limit = 2 // global cap across both patterns
	if err := run(context.Background(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if cfg.metrics.Hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 (global limit)", cfg.metrics.Hits.Load())
	}
	got := strings.Count(stdout.String(), "\n")
	if got != 2 {
		t.Fatalf("stdout lines = %d, want 2", got)
	}
}

func TestRunMultiPatternNoMatches(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cfg := multiPatternRunConfig(t, dir, []string{f}, []string{"zzz", "qqq"}, &stdout, nil)
	if err := run(context.Background(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if cfg.metrics.Hits.Load() != 0 {
		t.Fatalf("hits = %d, want 0", cfg.metrics.Hits.Load())
	}
}

// countingWriter records the number of Write calls so the pipe-fix test can
// assert per-hit flushing (each hit triggers a Write via the bufio flush).
type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return c.buf.Write(p)
}

func TestRunStreamFlushesPerHitWhenPiped(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("hit1\nhit2\nhit3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cw := &countingWriter{}
	cfg := runConfig{
		root:     dir,
		pattern:  "hit",
		archives: []string{f},
		txtMode:  true,
		workers:  1,
		stream:   true, // stream → stdoutIsTheSink true → streamFlush true
		started:  time.Now(),
		metrics:  &search.Metrics{},
		stdout:   cw, // non-TTY buffer stands in for a pipe
	}
	if err := run(context.Background(), cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 3 hits, each flushed individually → at least 3 Write calls to the
	// underlying writer (the bufio.Writer flushes per hit when streamFlush).
	if cw.writes < 3 {
		t.Fatalf("writes = %d, want >= 3 (per-hit flush when piped)", cw.writes)
	}
	if !strings.Contains(cw.buf.String(), "hit1") || !strings.Contains(cw.buf.String(), "hit3") {
		t.Fatalf("stdout = %q", cw.buf.String())
	}
}
