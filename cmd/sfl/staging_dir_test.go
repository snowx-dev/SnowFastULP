package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A regular file as a path component makes MkdirAll fail with ENOTDIR
// regardless of uid, so these tests are deterministic even when run as root
// (where 0500 permission bits would otherwise be bypassed).
func blockedDir(t *testing.T, base, name string) string {
	t.Helper()
	blocker := filepath.Join(base, name)
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "child")
}

func TestMakeStagingDirPrefersPrimary(t *testing.T) {
	base := t.TempDir()
	primary := filepath.Join(base, "parent")
	libDir := filepath.Join(base, "lib")

	workDir, err := makeStagingDir(primary, libDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := filepath.Dir(workDir); got != primary {
		t.Fatalf("workDir parent = %q, want primary %q", got, primary)
	}
	if !strings.HasPrefix(filepath.Base(workDir), "sfl-od-") {
		t.Fatalf("workDir base = %q, want sfl-od-* prefix", filepath.Base(workDir))
	}
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		t.Fatalf("workDir not created as dir: stat err=%v", err)
	}
}

// -od /tmp derives primary "/", which is unwritable; the staging dir must fall
// back to a subdir inside the library rather than crashing at the FS root.
func TestMakeStagingDirFallsBackIntoLibrary(t *testing.T) {
	base := t.TempDir()
	primary := blockedDir(t, base, "blocker")
	libDir := filepath.Join(base, "lib")

	workDir, err := makeStagingDir(primary, libDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := filepath.Dir(workDir); got != libDir {
		t.Fatalf("workDir parent = %q, want library %q", got, libDir)
	}
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		t.Fatalf("workDir not created as dir: stat err=%v", err)
	}
}

func TestMakeStagingDirExplicitErrorWhenNoneWritable(t *testing.T) {
	base := t.TempDir()
	primary := blockedDir(t, base, "blockerA")
	libDir := blockedDir(t, base, "blockerB")

	_, err := makeStagingDir(primary, libDir)
	if err == nil {
		t.Fatal("expected an error when neither path is writable")
	}
	msg := err.Error()
	for _, want := range []string{"could not create", primary, libDir, "-temp-dir"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message %q missing %q", msg, want)
		}
	}
}
