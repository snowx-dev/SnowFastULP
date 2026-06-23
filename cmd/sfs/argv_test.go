package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
)

// FlagSet sfs would register, passed explicit to SplitPositional to
// avoid the -count>1 panic from swapping flag.CommandLine
func newSFSTestFS() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("o", "", "output file")
	fs.Bool("txt", false, "txt mode")
	fs.Bool("silent", false, "silent")
	fs.Bool("clean", false, "clean")
	fs.Bool("debug", false, "debug")
	fs.Int("j", 0, "workers")
	return fs
}

func TestSplitPositional(t *testing.T) {
	fs := newSFSTestFS()
	flags, pos := cliargs.SplitPositional([]string{"needle", "-o", "out.txt", "-silent"}, fs)
	if len(pos) != 1 || pos[0] != "needle" {
		t.Fatalf("pos = %v", pos)
	}
	foundO := false
	for _, f := range flags {
		if f == "-o" || f == "out.txt" || f == "-silent" {
			foundO = true
		}
	}
	if !foundO {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitPositionalTxtFlag(t *testing.T) {
	fs := newSFSTestFS()
	flags, pos := cliargs.SplitPositional([]string{"-txt", "-silent", "pat"}, fs)
	if len(pos) != 1 || pos[0] != "pat" {
		t.Fatalf("pos = %v", pos)
	}
	hasTxt := false
	for _, f := range flags {
		if f == "-txt" || f == "-silent" {
			hasTxt = true
		}
	}
	if !hasTxt {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitPositionalPatternAfterFlags(t *testing.T) {
	fs := newSFSTestFS()
	flags, pos := cliargs.SplitPositional([]string{"-o", "hits.txt", "pattern"}, fs)
	if len(pos) != 1 || pos[0] != "pattern" {
		t.Fatalf("pos = %v", pos)
	}
	if len(flags) < 2 {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitPositionalDirAndPattern(t *testing.T) {
	fs := newSFSTestFS()
	_, pos := cliargs.SplitPositional([]string{"./library", "needle", "-o", "out.txt"}, fs)
	if len(pos) != 2 || pos[0] != "./library" || pos[1] != "needle" {
		t.Fatalf("pos = %v", pos)
	}
}

func TestParseSearchArgsPatternOnly(t *testing.T) {
	args, err := parseSearchArgs([]string{"needle"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Root != "." || args.Pattern != "needle" {
		t.Fatalf("args = %+v", args)
	}
}

func TestParseSearchArgsDirAndPattern(t *testing.T) {
	dir := t.TempDir()
	args, err := parseSearchArgs([]string{dir, "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Root != dir || args.Pattern != "needle" {
		t.Fatalf("args = %+v", args)
	}
}

func TestParseSearchArgsRejectsFileAsDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "logins.zst")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := parseSearchArgs([]string{file, "pattern"})
	if err == nil {
		t.Fatal("expected error when first arg is a regular file")
	}
	if !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("error = %v; want substring 'must be a directory'", err)
	}
}

func TestParseSearchArgsDirWithoutPattern(t *testing.T) {
	dir := t.TempDir()
	_, err := parseSearchArgs([]string{dir})
	if err == nil {
		t.Fatal("expected error for directory without pattern")
	}
}

func TestParseSearchArgsStarPattern(t *testing.T) {
	args, err := parseSearchArgs([]string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Pattern != "*" {
		t.Fatalf("pattern = %q, want *", args.Pattern)
	}
}

func TestParseSearchArgsMissing(t *testing.T) {
	_, err := parseSearchArgs(nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseSearchArgsTooMany(t *testing.T) {
	_, err := parseSearchArgs([]string{"a", "b", "c"})
	if err == nil {
		t.Fatal("expected error")
	}
}
