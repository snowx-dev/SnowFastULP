//go:build unix

package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildSFLSub builds the sfl binary for subprocess tests. The build dir is
// the test's own t.TempDir(): cleanup is the test process's job via t.Cleanup
// semantics, never the child's, so nothing leaks into the real temp root.
func buildSFLSub(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sfl")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfl: %v: %s", err, output)
	}
	return bin
}

// TestJSONOutBrokenPipe runs sfl as a subprocess with -json - (stdout)
// piped to a consumer that reads one event and then closes the pipe: the next
// stream write must hit EPIPE and mark the optional side stream dead, not kill
// the process with SIGPIPE. The SIGPIPE-ignore is Unix-only, so this file is
// build-tagged unix; on Windows a closed pipe fails the write directly.
func TestJSONOutBrokenPipe(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
		[]byte("URL: https://broken.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(buildSFLSub(t),
		"-config", configPath, "-no-tui", "-no-update-check",
		"-json=-", "-json-every", "200ms",
		"-o", out+string(os.PathSeparator), input)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Consume exactly one event (the start snapshot), then close the read end:
	// every later stream write hits a dead pipe.
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read start event: %v", err)
	}
	if !strings.Contains(line, `"event":"start"`) {
		t.Fatalf("first stream line = %q, want the start event", line)
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("sfl died on the broken JSON pipe: %v\nstderr:\n%s", err, stderr.String())
	}

	// Primary output complete despite the dead side stream.
	matches, err := filepath.Glob(filepath.Join(out, "sfl_*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("classic output missing: matches=%v err=%v\nstderr:\n%s", matches, err, stderr.String())
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "broken.example.com/login:u:p") {
		t.Fatalf("primary output incomplete: %q", body)
	}

	// No staging/temp debris: a SIGPIPE death skips defers and leaks them.
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sfl-spill-") || strings.HasPrefix(e.Name(), "sfl-od-") {
			t.Fatalf("temp debris left behind: %s", filepath.Join(os.TempDir(), e.Name()))
		}
	}
}
