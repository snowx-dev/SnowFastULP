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
	err := preflightOutputDir("-o", file, false)
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
	err := preflightOutputDir("-o", file+string(os.PathSeparator), false)
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
	err := preflightOutputDir("-odr", file, true)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("want not-a-directory error, got %v", err)
	}
	if fi, serr := os.Stat(file); serr != nil || fi.IsDir() {
		t.Fatalf("dry-run must not modify file: fi=%v err=%v", fi, serr)
	}
}

func TestPreflightOutputDirODCreatesMissing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "newlib")
	if err := preflightOutputDir("-od", target, false); err != nil {
		t.Fatalf("non-dry-run should create missing dir: %v", err)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("expected created dir, got fi=%v err=%v", fi, err)
	}
}

func TestPreflightOutputDirODRDryRunDoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "preview-missing")
	if err := preflightOutputDir("-odr", target, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create dir, stat err=%v", err)
	}
}

func TestPreflightOutputDirExistingDirOK(t *testing.T) {
	dir := t.TempDir()
	if err := preflightOutputDir("-o", dir, false); err != nil {
		t.Fatal(err)
	}
	if err := preflightOutputDir("-od", dir, false); err != nil {
		t.Fatal(err)
	}
	if err := preflightOutputDir("-odr", dir, true); err != nil {
		t.Fatal(err)
	}
}
