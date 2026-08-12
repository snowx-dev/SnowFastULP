package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResolveEnvDirPrefersOutputDir(t *testing.T) {
	cfg := runConfig{
		OutputDir:  filepath.Join("out"),
		LibraryDir: filepath.Join("lib"),
		Started:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	got := resolveEnvDir(cfg)
	want := filepath.Join("out", "sfl_20260102_030405_secrets")
	if got != want {
		t.Fatalf("resolveEnvDir = %q, want %q", got, want)
	}
}

func TestResolveEnvDirFallsBackToLibraryDir(t *testing.T) {
	cfg := runConfig{
		LibraryDir: filepath.Join("lib"),
		Started:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	got := resolveEnvDir(cfg)
	want := filepath.Join("lib", "sfl_20260102_030405_secrets")
	if got != want {
		t.Fatalf("resolveEnvDir = %q, want %q", got, want)
	}
}

func TestResolveEnvDirMatchesULPStamp(t *testing.T) {
	cfg := runConfig{
		OutputDir: t.TempDir(),
		Started:   time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
	}
	envDir := resolveEnvDir(cfg)
	outPath, err := createOutputPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stamp := "20260709_130000"
	if filepath.Base(envDir) != "sfl_"+stamp+"_secrets" {
		t.Fatalf("env dir base = %q", filepath.Base(envDir))
	}
	if filepath.Base(outPath) != "sfl_"+stamp+".txt" {
		t.Fatalf("ulp base = %q", filepath.Base(outPath))
	}
	if filepath.Dir(envDir) != filepath.Dir(outPath) {
		t.Fatalf("env and ulp should share parent: %q vs %q", envDir, outPath)
	}
}
