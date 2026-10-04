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
		"-s",
		"Deprecated, use default.",
		"-silent",
		"Deprecated, use default.",
		"-clean",
		"Strip URL schemes from output lines.",
		"-workers N",
		"Set search worker count.",
		"-j N",
		"Alias for -workers.",
		"-debug",
		"Write a debug log for this run.",
		"'gmail' | head",
		"-stats 'user@example'",
		"PATTERN '*' exports every line",
		"Parallel search over .zst archives (index-backed) or plain .txt files (-txt).",
		"Write results to FILE. With -f: per-term DIR",
		"Owns output: hits require -o FILE.",
		"[-o FILE] [-stats]",
		"Optional config:",
	}
	for _, s := range want {
		if !strings.Contains(help, s) {
			t.Fatalf("help is missing %q\n\n%s", s, help)
		}
	}

	if strings.Contains(help, "Flags:") {
		t.Fatalf("help should not use flag.PrintDefaults layout\n\n%s", help)
	}

	// -workers is the primary worker-count row (nerds tier); -j is only the
	// alias row in the devs tier — same treatment as the other tools.
	if idx := strings.Index(help, "Args (for devs):"); idx < 0 {
		t.Fatal("could not locate devs tier header in help")
	} else if strings.Contains(help[idx:], "-workers N") {
		t.Fatalf("-workers should not be an arg row in the devs tier:\n\n%s", help)
	}
	nerdsIdx := strings.Index(help, "Args (for nerds):")
	if nerdsIdx < 0 {
		t.Fatal("could not locate nerds tier header in help")
	}
	if !strings.Contains(help[nerdsIdx:], "-workers") {
		t.Fatalf("-workers should be the primary row in the nerds tier:\n\n%s", help)
	}
}
