package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// preflightOutputDir mirrors sfu's -o/-od/-odr guard so a file masquerading as a
// directory is rejected with a friendly message before the pipeline starts,
// instead of failing mid-run with a raw ENOTDIR.

func TestPreflightOutputDirORejectsPlainFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cleaned.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := preflightOutputDir("-o", file)
	if err == nil || !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("want dir-hint error, got %v", err)
	}
}

func TestPreflightOutputDirORejectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Trailing sep passes ResolveDir's dir-hint, but EnsureReady's Stat then
	// rejects the existing file.
	err := preflightOutputDir("-o", file+string(os.PathSeparator))
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want not-a-directory error, got %v", err)
	}
}

func TestPreflightOutputDirODRRejectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := preflightOutputDir("-odr", file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want not-a-directory error, got %v", err)
	}
	if fi, serr := os.Stat(file); serr != nil || fi.IsDir() {
		t.Fatalf("dry-run must not modify file: fi=%v err=%v", fi, serr)
	}
}

func TestPreflightOutputDirODDoesNotCreateMissing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "newlib")
	if err := preflightOutputDir("-od", target); err != nil {
		t.Fatalf("validation should accept missing dir: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("preflight must not create dir, stat err=%v", err)
	}
}

func TestPreflightJSONOutCollisionLeavesOutputDirAbsent(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.txt")
	if err := os.WriteFile(input, []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	cfg := runConfig{Input: input, OutputDir: out, JSONOut: input, RunStamp: "20260922_AAAAAA"}
	if err := preflightOutputDir("-o", out+string(os.PathSeparator)); err != nil {
		t.Fatal(err)
	}
	if err := validateJSONOutTarget(cfg); err == nil {
		t.Fatal("expected JSON collision validation error")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("rejected -json created output dir %q: %v", out, err)
	}
}

func TestPreflightOutputDirODRDryRunDoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "preview-missing")
	if err := preflightOutputDir("-odr", target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create dir, stat err=%v", err)
	}
}

func TestPreflightOutputDirExistingDirOK(t *testing.T) {
	dir := t.TempDir()
	if err := preflightOutputDir("-o", dir); err != nil {
		t.Fatal(err)
	}
	if err := preflightOutputDir("-od", dir); err != nil {
		t.Fatal(err)
	}
	if err := preflightOutputDir("-odr", dir); err != nil {
		t.Fatal(err)
	}
}
