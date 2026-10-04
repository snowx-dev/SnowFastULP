package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UX-audit #10: single-pattern/-stats -o always names a FILE; directory-shaped
// targets are refused before anything is created.
func TestValidateSingleOutputFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	existingDir := filepath.Join(dir, "adir")
	if err := os.Mkdir(existingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name, in, wantErr string
	}{
		{"plain file ok", existing, ""},
		{"new nested file ok", filepath.Join(dir, "new", "res.txt"), ""},
		{"trailing slash refused", existingDir + "/", "-o names a file in this mode; got a directory path"},
		{"backslash trailing refused", existingDir + `\`, "-o names a file in this mode; got a directory path"},
		{"existing dir refused", existingDir, "-o names a file in this mode; got an existing directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSingleOutputFile(tt.in)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want err containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// UX-audit #10: -f -o auto-creates a bare nonexistent directory, but a
// file-like name (extension-bearing basename) is refused; trailing / is the
// escape hatch; dotfiles (.cache) and existing dirs are exempt.
func TestValidateFileOutputDirFileLikeName(t *testing.T) {
	dir := t.TempDir()
	existingDotDir := filepath.Join(dir, "v1.2")
	if err := os.Mkdir(existingDotDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name, in, wantErr string
	}{
		{"bare no-dot ok", filepath.Join(dir, "results"), ""},
		{"dotfile ok", filepath.Join(dir, ".cache"), ""},
		{"dotted refused", filepath.Join(dir, "resfile.txt"), "-f: -o looks like a file name; add a trailing /"},
		{"dotted trailing slash ok", filepath.Join(dir, "resfile.txt") + "/", ""},
		{"existing dotted dir ok", existingDotDir, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			abs, err := validateFileOutputDir(tt.in)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				if abs == "" {
					t.Fatal("want abs path")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want err containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// No debris: a refused single-pattern -o must leave no created directory and
// must be a usage error (exit 2 shape comes from usage(); here we pin the
// validator-side invariant — nothing is created by the check itself).
func TestValidateSingleOutputFileCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "newdir", "res.txt")
	if err := validateSingleOutputFile(target); err != nil {
		t.Fatalf("new nested file target should validate; got %v", err)
	}
	if _, err := os.Stat(filepath.Dir(target)); !os.IsNotExist(err) {
		t.Fatalf("validator must not create parents; stat err = %v", err)
	}
}
