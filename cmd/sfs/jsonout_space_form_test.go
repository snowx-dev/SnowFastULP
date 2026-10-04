package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/testutil"
)

// D1 (sfs): the space form of -json (-json FILE) must consume FILE as
// the stream target like the = form, instead of leaking FILE into the
// positional arguments. The bare form keeps meaning stdout — and with the
// stream on, stdout carries NDJSON only: hits REQUIRE an explicit -o (they
// never share stdout with the stream, and no auto result file is generated).

func sfsJSONOutRoot(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	line := "https://example.com:user@example.com:needle\n"
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The empty config every run passes via -config.
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// runSFSCapture builds the sfs binary and runs it with stdout+stderr
// captured. Like runSFSUsage: per-test build, XDG env pointed at dir so
// config/history state cannot leak between tests.
func runSFSCapture(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sfs")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfs: %v: %s", err, output)
	}
	c := exec.Command(bin, args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
	)
	var outBuf, errBuf bytes.Buffer
	c.Stdout = &outBuf
	c.Stderr = &errBuf
	err := c.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfs: %v\nstderr:\n%s", err, errBuf.String())
	}
	return outBuf.String(), errBuf.String(), code
}

// The space form writes the stream to the named file; hits go to -o.
func TestJSONOutSpaceFormWritesFile(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	jsonl := filepath.Join(dir, "stats.jsonl")
	hits := filepath.Join(dir, "hits.txt")
	stdout, stderr, code := runSFSCapture(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-json", jsonl, "-o", hits, root, "needle")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	snaps := decodeSFSJSONL(t, mustReadSFS(t, jsonl))
	if snaps[0]["event"] != "start" || snaps[0]["tool"] != "sfs" {
		t.Fatalf("first stream event = %v/%v, want start/sfs", snaps[0]["event"], snaps[0]["tool"])
	}
	// The -o file carries the hits; stdout stays a pure stream, and the
	// auto sfs_results_*.txt generation is gone in json mode.
	data, err := os.ReadFile(hits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "needle") {
		t.Fatalf("-o file missing hits: %q", data)
	}
	if strings.Contains(stdout, "needle") {
		t.Fatalf("stdout must carry the JSON stream only, got hits on stdout:\n%s", stdout)
	}
	if leftovers, err := filepath.Glob(filepath.Join(dir, "sfs_results_*.txt")); err != nil || len(leftovers) != 0 {
		t.Fatalf("json mode must not generate a result file, got %v (%v)", leftovers, err)
	}
}

// -json without an explicit hit destination (-o / [sfs] o) is a usage
// error — bare and space forms alike. The message is exact and the -h hint
// follows (the usage() path).
func TestJSONOutWithoutOutputIsUsageError(t *testing.T) {
	const want = "sfs: -json owns stdout; give the hits a destination: -o FILE (or [sfs] o in config)"
	for name, args := range map[string][]string{
		"bare":  {"-json", "-txt", "root", "needle"},
		"space": {"-txt", "-json", "stats.jsonl", "root", "needle"},
		"forms": {"-txt", "-json=-", "root", "needle"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sfsJSONOutRoot(t, dir)
			stderr, code := runSFSUsage(t, dir, append([]string{
				"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
			}, args...)...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
			}
			lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
			if len(lines) != 2 || lines[0] != want || lines[1] != "run with -h for help" {
				t.Fatalf("stderr = %q, want the destination message + -h hint", stderr)
			}
		})
	}
}

// The bare form still means stdout: with -o the stream rides stdout (JSON
// only, ending in summary) and the hits go file-only to -o — never tee'd,
// never auto-generated.
func TestJSONOutWithOFileHitsFileOnly(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	probe := filepath.Join(dir, "stats.jsonl")
	hits := filepath.Join(dir, "hits.txt")
	stdout, stderr, code := runSFSCapture(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-json", "-o", hits, root, "needle")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		t.Fatalf("bare -json must not create %s: %v", probe, err)
	}
	// Every stdout line is stream JSON stamped for sfs; no raw hit lines.
	for _, ln := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if !strings.HasPrefix(ln, `{"v":1,"event":"`) || !strings.Contains(ln, `"tool":"sfs"`) {
			t.Fatalf("stdout line is not stream JSON: %q", ln)
		}
	}
	snaps := decodeSFSJSONL(t, stdout)
	if snaps[0]["event"] != "start" {
		t.Fatalf("first stream event = %v, want start", snaps[0]["event"])
	}
	if last := snaps[len(snaps)-1]; last["event"] != "summary" {
		t.Fatalf("last stream event = %v, want summary", last["event"])
	}
	data, err := os.ReadFile(hits)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "needle") != 2 {
		t.Fatalf("hit output %q must contain both hits, file-only and ordered", data)
	}
	if strings.Contains(stdout, "needle") {
		t.Fatalf("stdout must carry the JSON stream only:\n%s", stdout)
	}
	// Both streams are pipes in this subprocess, so stderr is not a terminal
	// and the live frame stays off. The stdout-terminal case is
	// TestSFSJSONOutDisplayFollowsStreamTarget.
	if !strings.Contains(stderr, "COMPLETE") {
		t.Fatalf("plain end-of-run summary missing from stderr:\n%s", stderr)
	}
	if strings.Contains(stderr, "\x1b[?1049h") {
		t.Fatalf("alt-screen escape leaked to stderr under -json:\n%s", stderr)
	}
	if leftovers, err := filepath.Glob(filepath.Join(dir, "sfs_results_*.txt")); err != nil || len(leftovers) != 0 {
		t.Fatalf("json mode must not generate a result file, got %v (%v)", leftovers, err)
	}
}

// The bare form rejects a non-positive -json-every with exit 2.
func TestJSONOutNonPositiveEveryIsUsageError(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hits := filepath.Join(dir, "hits.txt")
	stderr, code := runSFSUsage(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-txt", "-json", "-o", hits, "-json-every", "0", root, "needle")
	testutil.AssertShortUsageError(t, stderr, code, "must be positive")
}

// Config [sfs] json_out applies (config [sfs] o satisfies the destination);
// the CLI flag wins over the config value.
func TestSFSConfigJSONOutApplies(t *testing.T) {
	dir := t.TempDir()
	root := sfsJSONOutRoot(t, dir)
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[sfs]\njson_out = \"from-config.jsonl\"\no = \"hits.txt\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runSFSCapture(t, dir,
		"-config", cfg, "-no-update-check", "-txt", root, "needle")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	snaps := decodeSFSJSONL(t, mustReadSFS(t, filepath.Join(dir, "from-config.jsonl")))
	if snaps[0]["event"] != "start" {
		t.Fatalf("config json_out not applied: first event = %v", snaps[0]["event"])
	}
	// The config [sfs] o satisfies the destination: hits land there without
	// any CLI -o.
	if hits, err := os.ReadFile(filepath.Join(dir, "hits.txt")); err != nil || !strings.Contains(string(hits), "needle") {
		t.Fatalf("config o must carry the hits: %v %q", err, hits)
	}

	// CLI wins: the config file's stream must not reappear.
	if err := os.Remove(filepath.Join(dir, "from-config.jsonl")); err != nil {
		t.Fatal(err)
	}
	cliJSONL := filepath.Join(dir, "from-cli.jsonl")
	_, stderr, code = runSFSCapture(t, dir,
		"-config", cfg, "-no-update-check", "-txt", "-json", cliJSONL, root, "needle")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "from-config.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("CLI -json must win over config: config stream created anyway (%v)", err)
	}
	if snaps := decodeSFSJSONL(t, mustReadSFS(t, cliJSONL)); snaps[0]["event"] != "start" {
		t.Fatalf("CLI json_out stream missing: %v", snaps[0]["event"])
	}
}
