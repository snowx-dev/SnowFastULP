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
		"auto-generated CWD file.",
		"-s",
		"Stream results to stdout without the live screen.",
		"-silent",
		"Alias for -s.",
		"-clean",
		"Strip URL schemes from output lines.",
		"-j N",
		"Set search worker count.",
		"-sec-path",
		"-debug",
		"Write a debug log for this run.",
		"'gmail' -s | head",
		"'*' -since 5m -o recent.txt",
		"PATTERN '*' exports every line",
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
