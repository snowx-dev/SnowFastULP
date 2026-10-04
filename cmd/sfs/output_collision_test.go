package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureNoOutputCollisionEmptyOutFile(t *testing.T) {
	if err := ensureNoOutputCollision("", []string{"a.zst"}); err != nil {
		t.Fatalf("empty -o should be a no-op; got %v", err)
	}
}

func TestEnsureNoOutputCollisionDistinctPaths(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "hits.txt")
	arch := filepath.Join(dir, "logins.zst")
	if err := os.WriteFile(arch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureNoOutputCollision(out, []string{arch}); err != nil {
		t.Fatalf("distinct paths should be ok; got %v", err)
	}
}

func TestSFSRejectsOutputCollidingWithArchive(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "logins.zst")
	if err := os.WriteFile(arch, []byte("zst-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ensureNoOutputCollision(arch, []string{arch})
	if err == nil {
		t.Fatal("expected collision error")
	}
	if !strings.Contains(err.Error(), "would clobber") {
		t.Fatalf("error = %v; want substring 'would clobber'", err)
	}
}

func TestEnsureNoOutputCollisionRelativeAbsoluteAlias(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "logins.zst")
	if err := os.WriteFile(arch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := ensureNoOutputCollision("./logins.zst", []string{arch}); err == nil {
		t.Fatal("expected collision error for relative-vs-absolute alias")
	}
}

func TestEnsureNoOutputCollisionWhitespaceNameCollides(t *testing.T) {
	// P5-W8: paths are byte-preserved; only "" is the default. A
	// whitespace-containing path is a literal file name, so it must compare
	// verbatim — " hits.txt" is " hits.txt", not "hits.txt".
	dir := t.TempDir()
	arch := filepath.Join(dir, " hits.txt")
	if err := os.WriteFile(arch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// The path is byte-preserved; " hits.txt" is a literal file name, not
	// "hits.txt" — so a relative " hits.txt" must compare verbatim.
	if err := ensureNoOutputCollision(" hits.txt", []string{arch}); err == nil {
		t.Fatal("expected collision error for whitespace-containing literal name")
	}
	// A whitespace-only -o is still filesystem data (not the empty flag), so
	// it proceeds like any other non-colliding path.
	if err := ensureNoOutputCollision("   ", []string{arch}); err != nil {
		t.Fatalf("whitespace-only -o should be treated as a literal path; got %v", err)
	}
}
