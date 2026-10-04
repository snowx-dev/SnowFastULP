package main

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// TestRunCappedChunkIsPartial reports truncation so the caller can exit 3:
// a capped run returns nil (hits above the cap are valid output) but leaves
// ChunksCapped set for main's exit-code policy (internal/exitcode).
func TestRunCappedChunkIsPartial(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	writeZST(t, arch, []byte(
		"example.com:needle\nexample.com:needle\nexample.com:needle\n"))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:            dir,
		pattern:         "needle",
		archives:        []string{arch},
		workers:         1,
		maxHitsPerChunk: 2,
		stream:          true,
		started:         time.Now(),
		metrics:         metrics,
		stdout:          io.Discard,
	})
	if err != nil {
		t.Fatalf("cap-only truncation must still complete, got: %v", err)
	}
	if metrics.ChunksCapped.Load() != 1 {
		t.Fatalf("ChunksCapped = %d, want 1", metrics.ChunksCapped.Load())
	}
}

// TestBinaryExitCodeCappedIsPartial builds sfl's sibling policy: the real
// binary exits 3 (partial) when -max-hits-per-chunk truncated the results,
// with the TRUNCATED title in -stats mode.
func TestBinaryExitCodeCappedIsPartial(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sfs")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfs: %v: %s", err, output)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	writeZST(t, arch, []byte(
		"example.com:needle\nexample.com:needle\nexample.com:needle\n"))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatal(err)
	}

	runArgs := []string{
		"-config", filepath.Join(dir, "empty.toml"), "-no-update-check",
		"-stats", "-max-hits-per-chunk", "2", dir, "needle",
	}
	runSFS := func() (string, int) {
		c := exec.Command(bin, runArgs...)
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
		return string(out), code
	}
	if err := exec.Command("cp", "/dev/null", filepath.Join(dir, "empty.toml")).Run(); err != nil {
		t.Fatal(err)
	}
	out, code := runSFS()
	if code != exitcode.Partial {
		t.Fatalf("capped exit = %d, want %d\n%s", code, exitcode.Partial, out)
	}
	if !strings.Contains(out, "TRUNCATED") {
		t.Fatalf("capped -stats summary missing TRUNCATED title:\n%s", out)
	}
}
