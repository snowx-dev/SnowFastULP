package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveOutputModeDefaultStreamsToStdout(t *testing.T) {
	mode, err := resolveOutputMode("", false, t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if mode.OutFile != "" {
		t.Fatalf("default outFile = %q, want stdout (empty)", mode.OutFile)
	}
	if !mode.Stream {
		t.Fatal("default mode should stream to stdout")
	}
	if mode.Stats {
		t.Fatal("default mode should not be stats")
	}
	if mode.Generated {
		t.Fatal("default stream mode should not mark a generated file")
	}
}

func TestResolveOutputModeStatsGeneratesCWDResultFile(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 6, 27, 23, 38, 59, 0, time.Local)

	mode, err := resolveOutputMode("", true, dir, started)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "sfs_results_20260627-2338.txt")
	if mode.OutFile != want {
		t.Fatalf("outFile = %q, want %q", mode.OutFile, want)
	}
	if mode.Stream {
		t.Fatal("stats mode should not stream to stdout")
	}
	if !mode.Stats {
		t.Fatal("stats mode should be marked Stats")
	}
	if !mode.Generated {
		t.Fatal("stats mode should mark the output file as generated")
	}
}

func TestResolveOutputModeStatsAvoidsClobberingSameMinuteResult(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 6, 27, 23, 38, 0, 0, time.Local)
	mustWriteFile(t, filepath.Join(dir, "sfs_results_20260627-2338.txt"), "old")
	mustWriteFile(t, filepath.Join(dir, "sfs_results_20260627-2338_2.txt"), "old")

	mode, err := resolveOutputMode("", true, dir, started)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "sfs_results_20260627-2338_3.txt")
	if mode.OutFile != want {
		t.Fatalf("outFile = %q, want %q", mode.OutFile, want)
	}
}

func TestResolveOutputModeExplicitTeeWithoutStats(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "hits.txt")

	mode, err := resolveOutputMode(explicit, false, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if mode.OutFile != explicit {
		t.Fatalf("outFile = %q, want explicit %q", mode.OutFile, explicit)
	}
	if !mode.Stream {
		t.Fatal("-o without -stats should stream (tee) to stdout")
	}
	if mode.Stats {
		t.Fatal("-o without -stats should not be stats mode")
	}
	if mode.Generated {
		t.Fatal("explicit file output should not be marked generated")
	}
}

func TestResolveOutputModeStatsWithExplicitOutput(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "hits.txt")

	mode, err := resolveOutputMode(explicit, true, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if mode.OutFile != explicit {
		t.Fatalf("outFile = %q, want explicit %q", mode.OutFile, explicit)
	}
	if mode.Stream {
		t.Fatal("stats + -o should be file-only (no stdout stream)")
	}
	if !mode.Stats {
		t.Fatal("stats + -o should be marked Stats")
	}
	if mode.Generated {
		t.Fatal("explicit -o should not be marked generated")
	}
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
