package main

import (
	"bytes"
	"context"
	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	"github.com/snowx-dev/SnowFastULP/internal/index"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// M-10: a capped (-max-hits-per-chunk) run used to close the -json stream
// with a terminal "done" event while the process still exited 3, so machine
// consumers trusting the terminal event classified incomplete output as
// successful. The stream must carry an error terminal with code 3 followed by
// the truncated summary.
func TestSFSJSONOutTruncatedRunEmitsErrorTerminalWithCode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sfs")
	if output, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build sfs: %v: %s", err, output)
	}

	base := t.TempDir()
	dir := filepath.Join(base, "archives")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(base, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(base, "stats.jsonl")
	arch := filepath.Join(dir, "sample.zst")
	writeZST(t, arch, bytes.Repeat([]byte("needle line\n"), 10))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("cp", "/dev/null", filepath.Join(dir, "empty.toml")).Run(); err != nil {
		t.Fatal(err)
	}

	c := exec.Command(bin, "-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-json", jsonl, "-max-hits-per-chunk", "2", "-o", filepath.Join(outDir, "out.txt"), dir, "needle")
	c.Env = append(os.Environ(),
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
	if code != exitcode.Partial {
		t.Fatalf("capped exit = %d, want %d\n%s", code, exitcode.Partial, out)
	}

	snaps := decodeSFSJSONL(t, mustReadSFS(t, jsonl))
	terminalIdx := -1
	for i, s := range snaps {
		if e, _ := s["event"].(string); e == "error" || e == "done" || e == "interrupted" {
			terminalIdx = i
			break
		}
	}
	if terminalIdx < 0 {
		t.Fatalf("no terminal event in stream:\n%s", mustReadSFS(t, jsonl))
	}
	terminal := snaps[terminalIdx]
	if terminal["event"] != "error" {
		t.Fatalf("terminal event = %v, want error (truncated results)", terminal["event"])
	}
	if code, _ := terminal["code"].(float64); int(code) != exitcode.Partial {
		t.Fatalf("terminal code = %v, want %d", terminal["code"], exitcode.Partial)
	}
	sumIdx := -1
	for i := terminalIdx; i < len(snaps); i++ {
		if e, _ := snaps[i]["event"].(string); e == "summary" {
			sumIdx = i
			break
		}
	}
	if sumIdx < 0 {
		t.Fatalf("no summary after terminal:\n%s", mustReadSFS(t, jsonl))
	}
	sum, _ := snaps[sumIdx]["summary"].(map[string]any)
	if truncated, _ := sum["truncated"].(bool); !truncated {
		t.Fatalf("summary.truncated = %v, want true (block %v)", sum["truncated"], sum)
	}
	if _, ok := sum["search"]; ok {
		t.Fatalf("truncated summary must not nest search: %v", sum)
	}
	// Harmonized rollup: same shared blocks as live/done (and sfu/sfl).
	if lines, _ := sum["lines"].(map[string]any); lines["hits"] == nil {
		t.Fatalf("summary.lines.hits missing: %v", sum["lines"])
	}
	if bytes, _ := sum["bytes"].(map[string]any); bytes["read"] == nil || bytes["total"] == nil {
		t.Fatalf("summary.bytes read/total missing: %v", sum["bytes"])
	}
	if sources, _ := sum["sources"].(map[string]any); sources["archives"] == nil {
		t.Fatalf("summary.sources.archives missing: %v", sum["sources"])
	}
	if chunks, _ := sum["chunks"].(map[string]any); chunks["done"] == nil || chunks["total"] == nil {
		t.Fatalf("summary.chunks done/total missing: %v", sum["chunks"])
	}
}
