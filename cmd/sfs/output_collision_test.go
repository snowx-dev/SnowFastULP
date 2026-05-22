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
