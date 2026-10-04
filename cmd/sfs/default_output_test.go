package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFinalizeEmptyOutputRemovesGeneratedZeroHit proves a generated-default
// output that received no hits is unlinked (no 0-byte clutter in CWD) and the
// summary reports "(no matches)" instead of a path.
func TestFinalizeEmptyOutputRemovesGeneratedZeroHit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sfs_results_20260628-1200.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	out, removed := finalizeEmptyOutput(path, true, 0)
	if !removed {
		t.Fatalf("expected removed=true for generated zero-hit output")
	}
	if out != "(no matches)" {
		t.Fatalf("summaryOut = %q, want %q", out, "(no matches)")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file removed, stat err = %v", err)
	}
}

// TestFinalizeEmptyOutputKeepsFileWithHits keeps the file when hits were
// written, and TestFinalizeEmptyOutputKeepsExplicit keeps an explicit -o file
// even on zero hits — the user asked for that exact path.
func TestFinalizeEmptyOutputKeepsFileWithHits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sfs_results_20260628-1200.txt")
	if err := os.WriteFile(path, []byte("hit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, removed := finalizeEmptyOutput(path, true, 3)
	if removed {
		t.Fatalf("expected removed=false when hits > 0")
	}
	if out != path {
		t.Fatalf("summaryOut = %q, want %q", out, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file should remain: %v", err)
	}
}

func TestFinalizeEmptyOutputKeepsExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "explicit.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, removed := finalizeEmptyOutput(path, false, 0)
	if removed {
		t.Fatalf("explicit -o must never be removed")
	}
	if out != path {
		t.Fatalf("summaryOut = %q, want %q", out, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("explicit file should remain: %v", err)
	}
}

// TestOutputFooterShowsNoMatchesNote proves the "(no matches)" note rides the
// Output footer verbatim (not absolutized like a path).
func TestOutputFooterShowsNoMatchesNote(t *testing.T) {
	lines := renderOutputFooter("(no matches)", gradStart, gradEnd)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "(no matches)") {
		t.Fatalf("output footer missing note: %q", joined)
	}
	if strings.Contains(joined, string(os.PathSeparator)+"(no matches)") {
		t.Fatalf("note was treated as a path: %q", joined)
	}
}
