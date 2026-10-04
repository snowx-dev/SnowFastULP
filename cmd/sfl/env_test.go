package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveEnvDirPrefersOutputDir(t *testing.T) {
	cfg := runConfig{
		OutputDir:  filepath.Join("out"),
		LibraryDir: filepath.Join("lib"),
		RunStamp:   "20260102_test01",
	}
	got := resolveEnvDir(cfg)
	want := filepath.Join("out", "sfl_20260102_test01_secrets")
	if got != want {
		t.Fatalf("resolveEnvDir = %q, want %q", got, want)
	}
}

func TestResolveEnvDirFallsBackToLibraryDir(t *testing.T) {
	cfg := runConfig{
		LibraryDir: filepath.Join("lib"),
		RunStamp:   "20260102_test01",
	}
	got := resolveEnvDir(cfg)
	want := filepath.Join("lib", "sfl_20260102_test01_secrets")
	if got != want {
		t.Fatalf("resolveEnvDir = %q, want %q", got, want)
	}
}

func TestResolveEnvDirMatchesULPStamp(t *testing.T) {
	cfg := runConfig{
		OutputDir: t.TempDir(),
		RunStamp:  "20260102_test03",
	}
	envDir := resolveEnvDir(cfg)
	outPath, err := createOutputPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stamp := "20260102_test03"
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

// ensureRunStamp must respect an injected stamp (tests) and derive a
// YYYYMMDD_<id> stamp from the run clock otherwise — no time-of-day.
func TestEnsureRunStampDerivesDateAndID(t *testing.T) {
	cfg := runConfig{Started: time.Date(2026, 9, 20, 21, 30, 45, 0, time.UTC)}
	if err := ensureRunStamp(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RunStamp != "20260920_"+cfg.RunStamp[9:] || len(cfg.RunStamp) != 15 {
		t.Fatalf("run stamp = %q, want 20260920_<6-char-id>", cfg.RunStamp)
	}
	for _, r := range cfg.RunStamp[9:] {
		// crockford b32, no i/l/o/u
		if strings.ContainsRune("ilou", r) {
			t.Fatalf("run stamp id contains excluded letter: %q", cfg.RunStamp)
		}
	}
	// Injected stamps win.
	injected := runConfig{RunStamp: "20260102_test01"}
	if err := ensureRunStamp(&injected); err != nil {
		t.Fatal(err)
	}
	if injected.RunStamp != "20260102_test01" {
		t.Fatalf("injected stamp overwritten: %q", injected.RunStamp)
	}
}
