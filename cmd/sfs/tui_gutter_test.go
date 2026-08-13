package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputPathForSummaryPrefersRelative(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if got := outputPathForSummary("hits.txt"); got != "hits.txt" {
		t.Fatalf("outputPathForSummary(hits.txt) = %q, want hits.txt", got)
	}
}

func TestRenderOutputFooterShowsPathWithoutEllipsis(t *testing.T) {
	// path under another tree: footer shows absolute form, never ellipsize
	path := filepath.Join(t.TempDir(),
		"Data_Archive", "ulp", "Library", "hits", "gleeden.txt")
	lines := renderOutputFooter(path, gradStart, gradEnd)
	joined := strings.Join(lines, "\n")
	want, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(collapseRenderedText(joined), collapseRenderedText(want)) {
		t.Fatalf("missing output path %q:\n%s", want, joined)
	}
	if strings.ContainsRune(joined, '…') {
		t.Fatalf("output footer must not ellipsize:\n%s", joined)
	}
}
