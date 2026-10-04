package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dangling-symlink escape on the sfl side: a -json target that is a
// symlink to the not-yet-created history database must be refused, not
// resolved-and-truncated when the store opens.
func TestValidateJSONOutTargetRejectsSymlinkToHistoryDatabase(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	histDir := filepath.Join(dir, "hist")
	if err := os.MkdirAll(histDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(histDir, "history.sqlite3")
	link := filepath.Join(histDir, "link.jsonl")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"),
		RunStamp: "20260928_AAAAAA", History: true, HistoryPath: histDir,
		JSONOut: link,
	}
	err := validateJSONOutTarget(cfg)
	if err == nil {
		t.Fatal("symlink target to the history database must be refused")
	}
	if !strings.Contains(err.Error(), "overlaps history database") {
		t.Fatalf("wrong reason: %v", err)
	}
}

// The non-history segments of validateJSONOutTarget (input, input directory,
// password list, library directory, classic output) compared canonical
// spellings and SameFile only. Both pass when the target's final component is
// a symlink with a not-yet-existing referent — the canonical spelling of a
// dangling link is the link path itself — while the stream's create follows
// the link onto the referent. The referent chain must be checked against
// these segments the way the history segment already does.
func TestJSONOutDanglingSymlinkOntoNonHistorySegmentsRefused(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inputDir := filepath.Join(dir, "in.d")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}

	// Dangling link from outside landing on a not-yet-existing path inside
	// the library directory: the stream would create a file in the library.
	intoLib := filepath.Join(dir, "link-lib")
	if err := os.Symlink(filepath.Join(lib, "new.jsonl"), intoLib); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"),
		LibraryDir: lib, RunStamp: "20260928_AAAAAA", JSONOut: intoLib,
	}
	err := validateJSONOutTarget(cfg)
	if err == nil {
		t.Fatal("dangling symlink into the library directory must be refused")
	}
	if !strings.Contains(err.Error(), "overlaps library directory") {
		t.Fatalf("wrong reason: %v", err)
	}

	// Same shape for the input directory: the stream would smuggle a file
	// into the scanned tree. A directory input makes the input-directory
	// segment the relevant protection.
	intoInput := filepath.Join(dir, "link-input")
	if err := os.Symlink(filepath.Join(inputDir, "smuggled.txt"), intoInput); err != nil {
		t.Fatal(err)
	}
	cfg.Input = inputDir
	cfg.JSONOut = intoInput
	err = validateJSONOutTarget(cfg)
	if err == nil {
		t.Fatal("dangling symlink into the input directory must be refused")
	}
	if !strings.Contains(err.Error(), "overlaps input directory") {
		t.Fatalf("wrong reason: %v", err)
	}

	// A dangling link spelling the classic output's directory-adjacent spot
	// is fine as long as it lands nowhere protected: distinct referent stays
	// clear.
	clear := filepath.Join(dir, "link-clear")
	if err := os.Symlink(filepath.Join(dir, "stats.jsonl"), clear); err != nil {
		t.Fatal(err)
	}
	cfg.JSONOut = clear
	if err := validateJSONOutTarget(cfg); err != nil {
		t.Fatalf("unrelated dangling referent rejected: %v", err)
	}
}

// An existing library artifact reached through a symlink chain is destruction,
// not just pollution; the canonical compare already refuses the direct link,
// and a chained link (link → link → artifact) must be refused too.
func TestJSONOutChainedSymlinkOntoLibraryArtifactRefused(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(lib, "hits.jsonl")
	if err := os.WriteFile(artifact, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hop := filepath.Join(dir, "hop")
	if err := os.Symlink(artifact, hop); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(hop, link); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"),
		LibraryDir: lib, RunStamp: "20260928_AAAAAA", JSONOut: link,
	}
	err := validateJSONOutTarget(cfg)
	if err == nil {
		t.Fatal("chained symlink onto a library artifact must be refused")
	}
	if !strings.Contains(err.Error(), "overlaps library directory") {
		t.Fatalf("wrong reason: %v", err)
	}
}
