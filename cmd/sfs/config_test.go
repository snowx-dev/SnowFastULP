package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestConfigDirForPatternOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	lib := filepath.Join(dir, "library")
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	content := "[sfs]\ndir = \"library\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := config.Load(cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := f.ResolvedSFSDir()
	if err != nil {
		t.Fatal(err)
	}
	if resolved != lib {
		t.Fatalf("dir = %q want %q", resolved, lib)
	}

	args, err := parseSearchArgs([]string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	args.Root = resolved
	if args.Root != lib || args.Pattern != "needle" {
		t.Fatalf("args = %+v", args)
	}
}

func TestStripConfigArgvAllowsFlagParse(t *testing.T) {
	fs := newSFSTestFS()
	argv := config.StripConfigArgv([]string{"-config", filepath.Join(t.TempDir(), "x.toml"), "-silent", "pat"})
	flags, pos := cliargs.SplitPositional(argv, fs)
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pos) != 1 || pos[0] != "pat" {
		t.Fatalf("pos = %v", pos)
	}
}

func TestClampWorkersForFDAtLimit(t *testing.T) {
	t.Run("allowance below one worker pair errors", func(t *testing.T) {
		// A worker needs two descriptors; reject allowance == 1 rather than
		// floor-clamping to one worker whose descriptors exceed the budget.
		_, err := clampWorkersForFDAtLimit(4, 0, 17)
		if err == nil {
			t.Fatal("allowance < 2 with patternCount == 0 must error, got nil")
		}
		for _, want := range []string{"RLIMIT_NOFILE", "allowance"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v; want substring %q", err, want)
			}
		}
	})

	t.Run("no allowance with zero patterns errors", func(t *testing.T) {
		_, err := clampWorkersForFDAtLimit(4, 0, 16)
		if err == nil {
			t.Fatal("allowance < 2 with patternCount == 0 must error, got nil")
		}
		for _, want := range []string{"RLIMIT_NOFILE", "allowance"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v; want substring %q", err, want)
			}
		}
	})

	t.Run("no allowance with patterns errors", func(t *testing.T) {
		_, err := clampWorkersForFDAtLimit(4, 2, 18)
		if err == nil || !strings.Contains(err.Error(), "pattern output files exceed RLIMIT_NOFILE") {
			t.Fatalf("err = %v; want pattern-exceed error", err)
		}
	})

	t.Run("sufficient allowance clamps but succeeds", func(t *testing.T) {
		w, err := clampWorkersForFDAtLimit(64, 0, 64)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w != 24 { // (64-16)/2
			t.Errorf("w = %d; want 24", w)
		}
	})

	t.Run("workers below allowance untouched", func(t *testing.T) {
		w, err := clampWorkersForFDAtLimit(4, 0, 64)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w != 4 {
			t.Errorf("w = %d; want 4", w)
		}
	})
}
