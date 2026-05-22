package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputPathForSummaryResolvesAbsolute(t *testing.T) {
	rel := "hits.txt"
	want, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	if got := outputPathForSummary(rel); got != want {
		t.Fatalf("outputPathForSummary(%q) = %q, want %q", rel, got, want)
	}
}

func TestRenderOutputFooterShowsFullPath(t *testing.T) {
	// deeply-nested path, footer must show full path, never ellipsize
	path := filepath.Join(t.TempDir(),
		"Data_Archive", "ulp", "Library", "hits", "gleeden.txt")
	lines := renderOutputFooter(path, gradStart, gradEnd)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(collapseRenderedText(joined), path) {
		t.Fatalf("missing full output path:\n%s", joined)
	}
	if strings.ContainsRune(joined, '…') {
		t.Fatalf("output footer must not ellipsize:\n%s", joined)
	}
}
