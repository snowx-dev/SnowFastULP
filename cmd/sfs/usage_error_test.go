package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/testutil"
)

// buildSFSUsageBin builds the sfs binary for real-process usage-error checks.
func buildSFSUsageBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sfs")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfs: %v: %s", err, output)
	}
	return bin
}

func runSFSUsage(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	bin := buildSFSUsageBin(t)
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfs: %v\nstderr:\n%s", err, stderr.String())
	}
	return stderr.String(), code
}

// An undefined flag stays short (the flag package calls the short flag.Usage).
func TestUsageErrorBadFlagIsShort(t *testing.T) {
	dir := t.TempDir()
	stderr, code := runSFSUsage(t, dir, "-bogus-flag", "pattern")
	testutil.AssertShortUsageError(t, stderr, code, "flag provided but not defined")
}

// A bad -since window is an argv-shape usage error; output stays short.
func TestUsageErrorBadSinceIsShort(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFSUsage(t, dir, "-config", filepath.Join(dir, "empty.toml"), "-since", "not-a-window", "pattern")
	testutil.AssertShortUsageError(t, stderr, code, "sfs:")
}

// A rejected -f -o under the search root must exit 2 AND leave no directory
// materialized on disk: the under-root rejection runs before the -o dir is
// created.
func TestUsageErrorFileModeOutputUnderRootLeavesNoDir(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	terms := filepath.Join(dir, "terms.txt")
	if err := os.WriteFile(terms, []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(root, "out")
	stderr, code := runSFSUsage(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-f", terms, "-txt", "-o", outDir, root)
	testutil.AssertShortUsageError(t, stderr, code, "must be outside the search root")
	if _, err := os.Stat(outDir); !os.IsNotExist(err) {
		t.Fatalf("rejected -o dir must not be created; stat err = %v", err)
	}
}

// Explicit -h still prints the full help.
func TestHelpStillFullDump(t *testing.T) {
	dir := t.TempDir()
	bin := buildSFSUsageBin(t)
	cmd := exec.Command(bin, "-h")
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

// A -json file target that lives under the search root would be created
// (truncated) before the scan and sit inside the scanned tree; refuse with
// exit 2 before anything is written.
func TestUsageErrorJSONOutUnderSearchRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFSUsage(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-json", filepath.Join(root, "stats.jsonl"), "-o", filepath.Join(dir, "hits.txt"), root, "needle")
	testutil.AssertShortUsageError(t, stderr, code, "overlaps search root")
}

// A -json file target equal to the -o file would be truncated twice
// (stream open + hit output open) and the hits destroyed; refuse with exit 2.
func TestUsageErrorJSONOutEqualsOutputFile(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "hits.txt")
	stderr, code := runSFSUsage(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-json", out, "-o", out, root, "needle")
	testutil.AssertShortUsageError(t, stderr, code, "overlaps -o output")
}

// -f mode has its own per-term output; -json without -o DIR would leave
// the hits with nowhere to go. Refuse with exit 2.
func TestUsageErrorFileModeJSONOutRequiresOutDir(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	terms := filepath.Join(dir, "terms.txt")
	if err := os.WriteFile(terms, []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFSUsage(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-f", terms, "-json", root, "needle")
	testutil.AssertShortUsageError(t, stderr, code, "requires -o DIR")
}
