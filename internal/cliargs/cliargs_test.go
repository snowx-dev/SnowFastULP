package cliargs_test

import (
	"flag"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
)

func TestIsVersionRequest(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"-version"}, true},
		{[]string{"version"}, true},
		{[]string{"-h"}, false},
		{nil, false},
		// whole-argv scan, --version w/ extras still wins
		{[]string{"--version", "-no-tui"}, true},
		{[]string{"-no-tui", "--version"}, true},
	}
	for _, c := range cases {
		if got := cliargs.IsVersionRequest(c.argv); got != c.want {
			t.Errorf("IsVersionRequest(%v) = %v want %v", c.argv, got, c.want)
		}
	}
}

func TestIsHelpRequest(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"-h"}, true},
		{[]string{"--help"}, true},
		{[]string{"-help"}, true},
		{[]string{"in.txt", "-h"}, true},
		{[]string{"in.txt"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := cliargs.IsHelpRequest(c.argv); got != c.want {
			t.Errorf("IsHelpRequest(%v) = %v want %v", c.argv, got, c.want)
		}
	}
}

func newTestFS() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("o", "", "output")
	fs.Bool("zst", false, "compress")
	fs.Int("workers", 0, "workers")
	return fs
}

func TestSplitPositionalSimple(t *testing.T) {
	fs := newTestFS()
	flags, pos := cliargs.SplitPositional([]string{"in.txt", "-o", "out/"}, fs)
	if len(pos) != 1 || pos[0] != "in.txt" {
		t.Fatalf("pos = %v", pos)
	}
	if len(flags) != 2 || flags[0] != "-o" || flags[1] != "out/" {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitPositionalBoolDoesNotConsumeNext(t *testing.T) {
	fs := newTestFS()
	flags, pos := cliargs.SplitPositional([]string{"-zst", "in.txt"}, fs)
	if len(pos) != 1 || pos[0] != "in.txt" {
		t.Fatalf("pos = %v", pos)
	}
	if len(flags) != 1 || flags[0] != "-zst" {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitPositionalEqualsForm(t *testing.T) {
	fs := newTestFS()
	flags, pos := cliargs.SplitPositional([]string{"-workers=4", "in.txt"}, fs)
	if len(pos) != 1 || pos[0] != "in.txt" {
		t.Fatalf("pos = %v", pos)
	}
	if len(flags) != 1 || flags[0] != "-workers=4" {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitPositionalDoubleDashSeparator(t *testing.T) {
	fs := newTestFS()
	flags, pos := cliargs.SplitPositional([]string{"-o", "out/", "--", "-tricky"}, fs)
	if len(pos) != 1 || pos[0] != "-tricky" {
		t.Fatalf("pos = %v", pos)
	}
	if len(flags) != 2 {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitPositionalBareDashIsPositional(t *testing.T) {
	fs := newTestFS()
	_, pos := cliargs.SplitPositional([]string{"-"}, fs)
	if len(pos) != 1 || pos[0] != "-" {
		t.Fatalf("pos = %v", pos)
	}
}

func TestSplitPositionalNilFlagSetUsesCommandLine(t *testing.T) {
	prev := flag.CommandLine
	t.Cleanup(func() { flag.CommandLine = prev })
	flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
	flag.String("o", "", "output")

	flags, pos := cliargs.SplitPositional([]string{"-o", "out/", "x"}, nil)
	if len(pos) != 1 || pos[0] != "x" {
		t.Fatalf("pos = %v", pos)
	}
	if len(flags) != 2 || flags[1] != "out/" {
		t.Fatalf("flags = %v", flags)
	}
}

func TestSplitTrailingParen(t *testing.T) {
	cases := []struct {
		in, main, suffix string
	}{
		{"plain text", "plain text", ""},
		{"text with default (0=auto)", "text with default", " (0=auto)"},
		{"(only-parens)", "(only-parens)", ""},
		{"trailing space (x) ", "trailing space (x) ", ""},
	}
	for _, c := range cases {
		gotMain, gotSuffix := cliargs.SplitTrailingParen(c.in)
		if gotMain != c.main || gotSuffix != c.suffix {
			t.Errorf("SplitTrailingParen(%q) = (%q, %q) want (%q, %q)",
				c.in, gotMain, gotSuffix, c.main, c.suffix)
		}
	}
}
