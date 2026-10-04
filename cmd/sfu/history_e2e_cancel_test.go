//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// TestSFUE2E_AllHitDeleteInterruptExits130 drives the binary through the
// all-hit -history -del path and interrupts it mid-revalidation. Under the
// sampled fingerprint both read passes are tiny, so pass detection is not
// byte-based: the quarantine container (".snowfast-quarantine-*" beside the
// source) exists exactly between staging and validation completion, so its
// appearance is the signal to interrupt. A SIGINT during validation must
// restore the source and exit 130 promptly — the irreversible deletion must
// never ignore the first signal.
func TestSFUE2E_AllHitDeleteInterruptExits130(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	// The sampled validation reads only head+tail, so stretch each sampled
	// read past the poll interval to widen the interrupt window.
	t.Setenv("SNOWFAST_TEST_FP_READ_PAUSE_MS", "300")
	input := filepath.Join(dir, "big.txt")
	f, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	db := filepath.Join(dir, "history.sqlite3")
	// Seed the history DB in-process so the child goes straight down the
	// all-hit path without running the whole pipeline first.
	store, err := history.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{candidate.ID}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()

	cmd := sfuE2ECommand(t, bin, dir, input, "-history", "-history-path", db, "-del")
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

	// Poll for the staging container: its existence means the staged payload
	// is being re-fingerprinted (validation), with the 2x300ms read pauses
	// leaving ample time to land the signal mid-validation.
	deadline := time.Now().Add(60 * time.Second)
	signaled := false
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(dir, ".snowfast-quarantine-*"))
		if err == nil && len(matches) > 0 {
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatalf("signal: %v", err)
			}
			signaled = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !signaled {
		t.Fatal("never reached the validation pass (no quarantine container appeared)")
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 130 {
			t.Fatalf("exit = %v, want 130\noutput:\n%s", err, output.String())
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("run did not return after SIGINT (blocked read ignored the cancel)\noutput:\n%s", output.String())
	}
	if got := output.String(); !strings.Contains(got, "interrupted") {
		t.Fatalf("exit-130 run missing interrupt line:\n%s", got)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("interrupted deletion lost the source: %v", err)
	}
}

func readProcIORchr(pid int) (int64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "rchar: "); ok {
			return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		}
	}
	return 0, os.ErrNotExist
}
