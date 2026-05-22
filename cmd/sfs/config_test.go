package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestConfigDirForPatternOnly(t *testing.T) {
	dir := t.TempDir()
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
