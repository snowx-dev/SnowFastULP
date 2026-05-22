package main

import (
	"strings"
	"testing"
)

func TestRenderHelpUsesShortBeginnerFriendlyFlagDescriptions(t *testing.T) {
	help := renderHelp("sfu")

	want := []string{
		"-o DIR                Write output files to this folder.",
		"-od DIR               Write and dedup against old compressed results in this folder; this also compresses output.",
		"-zst                  Compress the output with zstd.",
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
