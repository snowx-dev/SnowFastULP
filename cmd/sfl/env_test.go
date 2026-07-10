package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResolveEnvRootClassic(t *testing.T) {
	out := t.TempDir()
	cfg := runConfig{
		OutputDir: out,
		Started:   time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
	}
	root, err := resolveEnvRoot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(out, "env", "202607091300")
	if root != want {
		t.Fatalf("root = %q want %q", root, want)
	}
}

func TestResolveEnvRootLibrary(t *testing.T) {
	lib := t.TempDir()
	cfg := runConfig{
		LibraryDir: lib,
		Started:    time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
	}
	root, err := resolveEnvRoot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(lib, "env", "202601020304")
	if root != want {
		t.Fatalf("root = %q want %q", root, want)
	}
}

func TestDryRunSkipsEnvCopier(t *testing.T) {
	cfg := runConfig{
		Env:       true,
		DryRun:    true,
		OutputDir: t.TempDir(),
		Started:   time.Now(),
	}
	root, err := resolveEnvRoot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if root == "" {
		t.Fatal("expected env root path for summary")
	}
}
