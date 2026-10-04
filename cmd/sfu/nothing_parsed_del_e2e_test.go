package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
)

// An all-reject run ends with NothingUsable (4): the run produced nothing
// usable, so it must never record the source as completed nor delete it —
// C-01: both happened after the exit-4 classification and destroyed the only
// evidence while the command reported failure.
func TestNothingParsedE2E_DoesNotDeleteSourceOrRecordHistory(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "invalid one\ninvalid two\n")
	db := filepath.Join(dir, "history.sqlite3")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runSFUE2EResult(t, bin, dir, input,
		"-del", "-o", out+string(os.PathSeparator), "-history", "-history-path", db)
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfu: %v\n%s", err, output)
	}
	if code != exitcode.NothingUsable {
		t.Fatalf("all-reject exit = %d, want %d\n%s", code, exitcode.NothingUsable, output)
	}
	if _, statErr := os.Stat(input); statErr != nil {
		t.Fatalf("all-reject run deleted the source: %v\n%s", statErr, output)
	}
	if strings.Contains(output, "Deleted") {
		t.Fatalf("all-reject run reported deletion in the summary:\n%s", output)
	}

	// The failed run must not have poisoned history: a second run over the
	// unchanged source must try again (exit 4 again), not skip as completed.
	output2, err2 := runSFUE2EResult(t, bin, dir, input,
		"-o", out+string(os.PathSeparator), "-history", "-history-path", db)
	code2 := 0
	if ee, ok := err2.(*exec.ExitError); ok {
		code2 = ee.ExitCode()
	} else if err2 != nil {
		t.Fatalf("second run: %v\n%s", err2, output2)
	}
	if code2 != exitcode.NothingUsable {
		t.Fatalf("second all-reject exit = %d, want %d\n%s", code2, exitcode.NothingUsable, output2)
	}
	if strings.Contains(output2, "already completed") {
		t.Fatalf("all-reject run recorded history; second run skipped the source:\n%s", output2)
	}
}

// Same guard on the history-less path: -del with no -history must also keep
// the source when nothing parsed.
func TestNothingParsedE2E_DoesNotDeleteSourceWithoutHistory(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "invalid one\ninvalid two\n")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := runSFUE2EResult(t, bin, dir, input,
		"-del", "-o", out+string(os.PathSeparator))
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfu: %v\n%s", err, output)
	}
	if code != exitcode.NothingUsable {
		t.Fatalf("all-reject exit = %d, want %d\n%s", code, exitcode.NothingUsable, output)
	}
	if _, statErr := os.Stat(input); statErr != nil {
		t.Fatalf("all-reject run deleted the source: %v\n%s", statErr, output)
	}
	if strings.Contains(output, "Deleted") {
		t.Fatalf("all-reject run reported deletion in the summary:\n%s", output)
	}
}
