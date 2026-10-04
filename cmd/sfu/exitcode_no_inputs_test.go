package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
)

// TestBinaryExitCodeNoInputsFound: an input dir with no .txt files discovers
// nothing — the same nothing-discovered class sfl exits 4 for. sfu used to
// exit 1 (runtime error) for it; the message text is unchanged, only the code
// moved to 4 per the shared exit-code policy.
func TestBinaryExitCodeNoInputsFound(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sfu")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build sfu: %v: %s", err, out)
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-.txt file proves the walk ran and found nothing usable — the
	// nothing-discovered arm, not a missing directory.
	if err := os.WriteFile(filepath.Join(empty, "notes.md"), []byte("nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", filepath.Join(dir, "empty.toml"),
		"-no-tui", "-no-update-check", empty)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"TMPDIR="+filepath.Join(dir, "tmp"),
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfu: %v\n%s", err, out)
	}
	if code != exitcode.NothingUsable {
		t.Fatalf("no-inputs exit = %d, want %d\n%s", code, exitcode.NothingUsable, out)
	}
	// The runtime-error wording is kept verbatim; only the code changed.
	if !strings.Contains(string(out), "no .txt files found under: "+empty) {
		t.Fatalf("message must stay truthful and unchanged:\n%s", out)
	}
}
