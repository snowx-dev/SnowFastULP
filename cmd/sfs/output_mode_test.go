package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveOutputModeDefaultGeneratesCWDResultFile(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 6, 27, 23, 38, 59, 0, time.Local)

	mode, err := resolveOutputMode("", false, dir, started)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "sfs_results_20260627-2338.txt")
	if mode.OutFile != want {
		t.Fatalf("outFile = %q, want %q", mode.OutFile, want)
	}
	if mode.Stream {
		t.Fatal("default mode should not stream to stdout")
	}
	if !mode.Generated {
		t.Fatal("default mode should mark the output file as generated")
	}
}

func TestResolveOutputModeDefaultAvoidsClobberingSameMinuteResult(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 6, 27, 23, 38, 0, 0, time.Local)
	mustWriteFile(t, filepath.Join(dir, "sfs_results_20260627-2338.txt"), "old")
	mustWriteFile(t, filepath.Join(dir, "sfs_results_20260627-2338_2.txt"), "old")

	mode, err := resolveOutputMode("", false, dir, started)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "sfs_results_20260627-2338_3.txt")
	if mode.OutFile != want {
		t.Fatalf("outFile = %q, want %q", mode.OutFile, want)
	}
}

func TestResolveOutputModeExplicitOutputWinsOverGeneratedDefault(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "hits.txt")

	mode, err := resolveOutputMode(explicit, false, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if mode.OutFile != explicit {
		t.Fatalf("outFile = %q, want explicit %q", mode.OutFile, explicit)
	}
	if mode.Stream {
		t.Fatal("explicit file output should not be stdout stream mode")
	}
	if mode.Generated {
		t.Fatal("explicit file output should not be marked generated")
	}
}

func TestResolveOutputModeStreamUsesStdout(t *testing.T) {
	mode, err := resolveOutputMode("", true, t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if mode.OutFile != "" {
		t.Fatalf("stream mode outFile = %q, want stdout", mode.OutFile)
	}
	if !mode.Stream {
		t.Fatal("stream mode should be marked as stdout streaming")
	}
}

func TestStreamRequestedAcceptsSilentAlias(t *testing.T) {
	if !streamRequested(false, true) {
		t.Fatal("-silent should remain an alias for stream mode")
	}
	if !streamRequested(true, false) {
		t.Fatal("-s should request stream mode")
	}
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
