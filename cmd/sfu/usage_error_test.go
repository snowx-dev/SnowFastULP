package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/testutil"
)

// Two positional inputs is the arity error; output stays short.
func TestUsageErrorArityIsShort(t *testing.T) {
	dir := t.TempDir()
	stderr, code := runSFUSpaceForm(t, dir, "", "-no-tui", "-no-update-check", "first", "second")
	testutil.AssertShortUsageError(t, stderr, code, "expected exactly one input path")
}

// An undefined flag stays short (the flag package calls the short flag.Usage).
func TestUsageErrorBadFlagIsShort(t *testing.T) {
	dir := t.TempDir()
	stderr, code := runSFUSpaceForm(t, dir, "", "-no-tui", "-no-update-check", "-bogus-flag")
	testutil.AssertShortUsageError(t, stderr, code, "flag provided but not defined")
}

// Explicit -h still prints the full help.
func TestHelpStillFullDump(t *testing.T) {
	dir := t.TempDir()
	bin := buildSFUE2E(t)
	cfg := filepath.Join(dir, "empty.toml")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", cfg, "-h")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
	)
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("-h failed: %v\n%s", err, stdout)
	}
	for _, want := range []string{"Usage:", "Args:"} {
		if !strings.Contains(string(stdout), want) {
			t.Fatalf("-h output missing %q:\n%s", want, stdout)
		}
	}
}
