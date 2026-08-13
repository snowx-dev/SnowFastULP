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
		"-stats",
		"Live progress screen",
		"Not valid with -sec.",
		"-s",
		"Deprecated alias for default stream-to-stdout mode.",
		"-silent",
		"Alias for -s.",
		"-clean",
		"Strip URL schemes from output lines.",
		"-j N",
		"Set search worker count.",
		"-sec-path",
		"-debug",
		"Write a debug log for this run.",
		"'gmail' | head",
		"-stats 'user@example'",
		"PATTERN '*' exports every line",
		"Hits stream to stdout by default.",
		"[-o FILE] [-stats]",
		"Optional config:",
		"relative paths resolve against process CWD, like flags",
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
