package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// TestHistoryE2E_QualityRejectsStillRecord pins the 2026-09-30 product
// decision: a source whose credentials are rejected by the shared-parser
// emit gate is a parse-quality outcome — the issue is reported, but the
// source still records in history alongside its clean siblings, and a rerun
// skips both. Rejected lines are deterministic, so a recorded dirty source
// re-extracts identically; -del may delete it per the same decision.
func TestHistoryE2E_QualityRejectsStillRecord(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in")
	if err := os.MkdirAll(input, 0o700); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(input, "clean.zip")
	bad := filepath.Join(input, "bad.zip")
	writeHistoryZip(t, clean, "victim/Passwords.txt", "URL: clean.example\nUSER: user\nPASS: pw\n")
	writeHistoryZip(t, bad, "victim/Passwords.txt", "URL: bad.example\nUSER: baduser\nPASS: "+strings.Repeat("p", 65)+"\n")
	cleanCandidate, err := history.FingerprintFile(context.Background(), clean, nil)
	if err != nil {
		t.Fatal(err)
	}
	badCandidate, err := history.FingerprintFile(context.Background(), bad, nil)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "history.sqlite3")

	// Run 1: the bad zip's password>64 credential is gate-rejected (exit 3,
	// issue logged), but both sources record.
	r := newExitRun(t)
	r.out = "" // -od replaces -o; the two flags are mutually exclusive
	lib := filepath.Join(dir, "lib") + string(os.PathSeparator)
	stderr, _, code := r.runSandboxed(t, "-od", lib, "-history", "-history-path", db, input)
	if code != exitcodePartial {
		t.Fatalf("first run error = %v, want partial outcome 3", code)
	}
	if !strings.Contains(stderr, "issues") {
		t.Fatalf("first run summary missing issues footer:\n%s", stderr)
	}
	issueLogs, _ := filepath.Glob(filepath.Join(dir, "lib", "sfl-issues-*.log"))
	if len(issueLogs) != 1 {
		t.Fatalf("first run wrote %d issue logs, want 1", len(issueLogs))
	}
	raw, err := os.ReadFile(issueLogs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "password>64") {
		t.Fatalf("issue log missing the gate reject reason:\n%s", raw)
	}
	assertHistoryHit(t, db, cleanCandidate.ID, true)
	assertHistoryHit(t, db, badCandidate.ID, true)

	// Run 2: both sources skip — the dirty source is deterministic, so the
	// recorded identity must still authorize the skip. A pure all-skip rerun
	// prints sfu's one-line summary instead of the recap box.
	r2 := newExitRun(t)
	r2.out = ""
	stderr, _, code = r2.runSandboxed(t, "-od", lib, "-history", "-history-path", db, input)
	if code != 0 {
		t.Fatalf("second run error = %v, want clean exit 0\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "✓ history: 2 sources already completed · nothing to process") {
		t.Fatalf("second run did not print the sfu-parity one-line summary:\n%s", stderr)
	}
	if strings.Contains(stderr, "History") || strings.Contains(stderr, "Input") {
		t.Fatalf("second run must not render the recap box:\n%s", stderr)
	}
}
