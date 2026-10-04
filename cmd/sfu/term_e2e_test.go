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

// TestSFUE2E_TermExits143 pins the conventional signal split on the real
// binary: SIGTERM takes the same graceful fileabort path as SIGINT but exits
// 143 (128+15), not the SIGINT 130.
func TestSFUE2E_TermExits143(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "input.txt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
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

	time.Sleep(700 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 143 {
			t.Fatalf("exit = %v, want 143\noutput:\n%s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("blocked read ignored SIGTERM; run hung instead of exiting 143\noutput:\n%s", output.String())
	}
	if got := output.String(); !strings.Contains(got, "interrupted") {
		t.Fatalf("missing interrupted line:\n%s", got)
	}
}
