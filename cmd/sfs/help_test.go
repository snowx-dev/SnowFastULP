package main

import (
	"strings"
	"testing"
)

func TestRenderHelpMatchesSFULayout(t *testing.T) {
	help := renderHelp("sfs")

	want := []string{
		"SnowFastSearch",
		"Usage:",
		"Examples:",
		"Args:",
		"Args (for nerds):",
		"Args (for devs):",
		"-o FILE",
		"Write results to this file instead of stdout.",
		"-silent",
		"Use plain text output instead of the live screen.",
		"-clean",
		"Strip URL schemes from output lines.",
		"-j N",
		"Set search worker count.",
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
