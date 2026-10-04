//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// findDebugArtifact locates the single debug artifact matching pattern in dir.
func findDebugArtifact(t *testing.T, dir, pattern string) (string, string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly one %s under %s, got %v", pattern, dir, matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return matches[0], string(raw)
}

// The interrupt seam (SIGINT → exit 130) must flush the -debug-reject
// recorder: rejects sit in a 256KB buffer that only the deferred Close ever
// flushed, and os.Exit skips defers — so a Ctrl-C, the exact moment you go
// read the reject file, lost every recorded line. The input directory holds
// one real file whose malformed line is recorded (pending) and a FIFO whose
// read blocks forever after, making the pending-tail precondition
// deterministic. Whether the child exits via the graceful seam or the
// cleanup-timeout force-exit, the code is 130 either way — and the buffered
// reject line must be on disk. Fails pre-fix: the reject file is empty.
// (The -debug log itself needs no such test: every engine write path —
// header, rationale, PHASE markers, progress, termination — flushes
// immediately, so its buffer is empty at every reachable seam; the reject
// recorder is the only artifact that buffers without flushing.)
func TestSFUE2E_InterruptFlushesRejectRecorder(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inDir := filepath.Join(dir, "inputs")
	if err := os.MkdirAll(inDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inDir, "mixed.txt"),
		[]byte("https://a.example.com:user:p\nnot-a-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The FIFO stalls the parse read after mixed.txt is consumed: no EOF, so
	// the run only ever ends through the interrupt paths.
	fifo := filepath.Join(inDir, "stall.txt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	cmd := sfuE2ECommand(t, bin, dir, inDir, "-debug-reject")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	}()

	// Give the child time to record the reject and block on the FIFO, then a
	// single SIGINT. Graceful unstick or 5s-grace cleanup timeout: exit 130.
	time.Sleep(1200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}
	code := waitE2EExit(t, cmd, &strings.Builder{}, 20*time.Second)
	if code != 130 {
		t.Fatalf("exit = %d, want 130", code)
	}
	_, content := findDebugArtifact(t, dir, "sfu-rejected-*.txt")
	if !strings.Contains(content, "not-a-line") {
		t.Fatalf("reject file lost its buffered line on the interrupt seam: %q", content)
	}
}
