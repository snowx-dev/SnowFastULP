package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSecretsPathDefaults(t *testing.T) {
	if got := resolveSecretsPath("", "/out", ""); got != "/out/sfl-secrets.sqlite" {
		t.Fatalf("output-dir default = %q", got)
	}
	if got := resolveSecretsPath("", "", "/lib"); got != "/lib/sfl-secrets.sqlite" {
		t.Fatalf("library-dir default = %q", got)
	}
	if got := resolveSecretsPath("/x/custom.db", "/out", ""); got != "/x/custom.db" {
		t.Fatalf("explicit file path = %q", got)
	}
	if got := resolveSecretsPath("", "", ""); got != secretsDBName {
		t.Fatalf("cwd fallback = %q", got)
	}
}

// A -secrets-path ending in a separator is a directory: the DB filename is
// appended, mirroring -o/-od semantics.
func TestResolveSecretsPathTrailingSlashIsDir(t *testing.T) {
	if got := resolveSecretsPath("/some/dir/", "/out", ""); got != "/some/dir/sfl-secrets.sqlite" {
		t.Fatalf("trailing-slash dir = %q", got)
	}
}

// A -secrets-path that points at an existing directory also gets the filename
// appended, even without a trailing slash.
func TestResolveSecretsPathExistingDirIsDir(t *testing.T) {
	dir := t.TempDir()
	got := resolveSecretsPath(dir, "/out", "")
	want := filepath.Join(dir, secretsDBName)
	if got != want {
		t.Fatalf("existing dir = %q, want %q", got, want)
	}
}

// A -secrets-path that doesn't exist and has no trailing slash is treated as a
// literal file path (legacy behavior), so an explicit DB filename still works.
func TestResolveSecretsPathMissingNoSlashIsFile(t *testing.T) {
	got := resolveSecretsPath(filepath.Join(os.TempDir(), "nested", "custom.db"), "/out", "")
	want := filepath.Join(os.TempDir(), "nested", "custom.db")
	if got != want {
		t.Fatalf("missing file path = %q, want %q", got, want)
	}
}
