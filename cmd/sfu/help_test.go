package main

import (
	"strings"
	"testing"
)

func TestRenderHelpUsesShortBeginnerFriendlyFlagDescriptions(t *testing.T) {
	help := renderHelp("sfu")

	want := []string{
		"-o DIR                Write output files to this folder.",
		"-od DIR               Antipublic Library. Write and dedup against previous compressed results in this folder; this also compresses output.",
		"-zst                  One time compress the output with zstd.",
		"-del                  Delete input .txt files after a successful run.",
		"-no-uri               Save only host:login:password.",
		"-no-tui               Use plain text output instead of the live screen.",
		"-workers N            Set parser worker count.",
		"-dedup N              Set dedup worker count.",
		"-buckets N            Set the number of temp buckets.",
		"-temp-dir PATH        Store temp files in this folder.",
		"-split-zst N          Split compressed output every N unique lines.",
		"-loose                Accept more input formats, with less strict parsing.",
		"-no-encoding-sniff    Skip encoding checks and read files as UTF-8.",
		"-debug                Write a debug log for this run.",
		"-debug-reject         Write rejected input lines to a debug file.",
	}
	for _, s := range want {
		if !strings.Contains(help, s) {
			t.Fatalf("help is missing %q\n\n%s", s, help)
		}
	}

	tooVerbose := []string{
		"timestamped sfu_<ts>",
		".idx sidecars",
		"JSON / form-metadata",
		"A-B benchmark",
		"line-or-byte-offset",
	}
	for _, s := range tooVerbose {
		if strings.Contains(help, s) {
			t.Fatalf("help still contains verbose detail %q\n\n%s", s, help)
		}
	}
}

// -odr is documented in the nerdy tier so library-mode users can find the
// dry-run preview without it cluttering the beginner block.
func TestRenderHelpIncludesODRInNerdyTier(t *testing.T) {
	help := renderHelp("sfu")
	for _, want := range []string{"-odr", "preview what a run would add to the library"} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q\n\n%s", want, help)
		}
	}
	// it must NOT be in the primary tier (beginner block keeps -o/-od/-zst/...)
	// — the nerdy tier starts at this header, so -odr must appear after it.
	nerdyStart := strings.Index(help, "Args (for nerds):")
	if nerdyStart < 0 {
		t.Fatal("could not locate nerdy tier header in help")
	}
	if idx := strings.Index(help[:nerdyStart], "-odr"); idx >= 0 {
		t.Errorf("-odr should be in the nerdy tier, not the primary block:\n%s", help)
	}
}
