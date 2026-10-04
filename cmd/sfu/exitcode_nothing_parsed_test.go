package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
)

// TestBinaryExitCodeNothingParsed: a run that reads lines but parses none
// exits 4 (nothing usable per internal/exitcode) with an honest title.
func TestBinaryExitCodeNothingParsed(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sfu")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build sfu: %v: %s", err, out)
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "garbage.txt")
	if err := os.WriteFile(input, []byte("not a credential\nalso not\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", filepath.Join(dir, "empty.toml"),
		// space form now works too (D1 fix); the = form keeps this test
		// independent of splitter behavior.
		"-no-tui", "-no-update-check", "-json="+filepath.Join(dir, "stats.jsonl"),
		"-o", filepath.Join(dir, "out")+string(filepath.Separator), input)
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
		t.Fatalf("nothing-parsed exit = %d, want %d\n%s", code, exitcode.NothingUsable, out)
	}
	if !strings.Contains(string(out), "NOTHING PARSED") {
		t.Fatalf("summary title must admit nothing parsed:\n%s", out)
	}
	// The NDJSON terminal error event carries the exit code additively.
	stream, err := os.ReadFile(filepath.Join(dir, "stats.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var sawError, sawSummary bool
	for _, line := range strings.Split(strings.TrimSpace(string(stream)), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", line, err)
		}
		switch ev["event"] {
		case "error":
			sawError = true
			if ev["code"] != float64(exitcode.NothingUsable) {
				t.Fatalf("terminal code = %v, want %d", ev["code"], exitcode.NothingUsable)
			}
		case "summary":
			sawSummary = true
			if _, has := ev["code"]; has {
				t.Fatalf("summary event must not carry a code: %v", ev)
			}
		}
	}
	if !sawError || !sawSummary {
		t.Fatalf("stream missing error terminal and/or summary:\n%s", stream)
	}
}
