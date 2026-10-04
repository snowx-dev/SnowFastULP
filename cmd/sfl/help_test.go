package main

import (
	"strings"
	"testing"
)

func TestRenderHelpMatchesSnowFastLayout(t *testing.T) {
	help := renderHelp("sfl")

	want := []string{
		"SnowFastLog",
		"Usage:",
		"Examples:",
		"Args:",
		"Args (for nerds):",
		"Args (for devs):",
		// synopsis convention: CAPS placeholders + [] optionals, 4-space indent
		// (uniform with sfu/sfs)
		"Usage:\n    sfl INPUT_PATH [-o DIR]",
		"    sfl INPUT_PATH [-od DIR] [-p PASSWORD_OR_FILE]",
		"-o DIR",
		"Write extracted ULP lines to this folder. Must be an existing directory or end with `/` to create new dir.",
		"-od DIR",
		"Ingest extracted ULP lines into a new or existing sfu library.",
		"-p PASSWORD_OR_FILE",
		"Archive password or password-list file (one per line).",
		"-no-uri",
		"Save only host:login:password.",
		"-workers N",
		"Set parser/archive worker count.",
		"-debug",
		"Write a debug log for this run.",
		"Optional config:",
		"Capture env files, API key files, and tdata dirs",
		"Full list in docs",
	}
	for _, s := range want {
		if !strings.Contains(help, s) {
			t.Fatalf("help is missing %q\n\n%s", s, help)
		}
	}
	if strings.Contains(help, "Flags:") {
		t.Fatalf("help should not use flag.PrintDefaults layout\n\n%s", help)
	}
}

func TestRenderHelpIncludesHistoryControls(t *testing.T) {
	help := renderHelp("sfl")
	for _, want := range []string{"-history", "-history-path PATH", "Use this history database file (does not enable history)"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}
