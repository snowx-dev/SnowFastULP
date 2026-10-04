//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSFUE2E_InterruptUnblocksBlockedInputRead proves the fileabort/grace
// wiring: a fingerprint read blocked on an empty FIFO (the slow-storage seam)
// is closed by the first SIGINT via the fileabort registry, the run reports
// interrupted and exits 130 instead of hanging until a second Ctrl-C.
func TestSFUE2E_InterruptUnblocksBlockedInputRead(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	// CollectInputs requires a .txt suffix, so the FIFO is named input.txt.
	fifo := filepath.Join(dir, "input.txt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// An O_RDWR fd holds both FIFO ends in-process: it never blocks on open
	// and the child's read blocks forever after the first line (no EOF),
	// which is exactly the stuck-read seam.
	holder, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.WriteString("https://example.com:user:pass\n"); err != nil {
		t.Fatal(err)
	}

	cmd := sfuE2ECommand(t, bin, dir, fifo, "-history", "-history-path", filepath.Join(dir, "history.sqlite3"))
	var output strings.Builder
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	}()

	// Give the child time to reach the blocked read, then send exactly one
	// SIGINT — the graceful path must unstick itself.
	time.Sleep(700 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
			t.Fatalf("exit = %v, want 130\noutput:\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("blocked read ignored the first signal; run hung instead of exiting 130\noutput:\n%s", output.String())
	}
	if got := output.String(); !strings.Contains(got, "interrupted") {
		t.Fatalf("missing interrupted line:\n%s", got)
	}
}
