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
		"-o DIR",
		"Write extracted ULP lines to this folder.",
		"-od DIR",
		"Ingest extracted ULP lines into an existing sfu library.",
		"-p PASSWORD_OR_FILE",
		"Archive password or password-list file.",
		"-no-uri",
		"Save only host:login:password.",
		"-workers N",
		"Set parser/archive worker count.",
		"-debug",
		"Write a debug log for this run.",
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
