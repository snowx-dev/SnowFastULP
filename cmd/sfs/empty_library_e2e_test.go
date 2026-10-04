package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinaryEmptyLibraryExitsFourWithHint pins the empty-library policy: a
// library path with no .zst files discovered nothing usable, so it exits 4
// (the shared internal/exitcode contract, matching sfu/sfl) — but the
// message must be actionable (wrong path vs empty library, and the -od
// ingest reminder) instead of a bare file list.
func TestBinaryEmptyLibraryExitsFourWithHint(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sfs")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfs: %v: %s", err, output)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	c := exec.Command(bin, "-config", filepath.Join(dir, "empty.toml"), "-no-update-check", lib, "needle")
	c.Dir = dir
	c.Env = append(c.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"TMPDIR="+filepath.Join(dir, "tmp"),
	)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfs: %v\n%s", err, out)
	}
	if code != 4 {
		t.Fatalf("empty-library exit = %d, want 4\n%s", code, out)
	}
	msg := string(out)
	for _, want := range []string{"no .zst files under", "path may be wrong", "library is empty", "sfl -od"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("empty-library message missing %q:\n%s", want, msg)
		}
	}
}
