package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// The sfl emit gate owns built-in grammar rejects before library ingest. A
// password over the shared cap is reported against the source, never staged
// into the library, while valid siblings still commit. The reject is a
// parse-quality outcome: since the 2026-09-30 decision it no longer
// withholds history — the source records — and the run still exits partial
// so the issues log gets attention.
func TestEmitGateRejectsBeforeLibraryIngest(t *testing.T) {
	r := newExitRun(t)
	r.out = "" // -od replaces -o; the two flags are mutually exclusive
	input := filepath.Join(r.dir, "Passwords.txt")
	longPassword := strings.Repeat("p", 65)
	body := "URL: a.com\nUSER: u\nPASS: " + longPassword + "\n" +
		"URL: b.com\nUSER: u2\nPASS: short\n"
	if err := os.WriteFile(input, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	lib := filepath.Join(r.dir, "lib") + string(os.PathSeparator)
	db := filepath.Join(r.dir, "history.sqlite3")
	joutPath := filepath.Join(r.dir, "stats.ndjson")
	stderr, _, code := r.runSandboxed(t, "-debug", "-debug-reject", "-json", joutPath, "-od", lib, "-history", "-history-path", db, "-no-tui", input)
	if code != exitcodePartial {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodePartial, stderr)
	}

	streamRaw, err := os.ReadFile(joutPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(streamRaw), "library ingest rejected") {
		t.Fatalf("built-in reject leaked into library ingest:\n%s", streamRaw)
	}

	entries, err := os.ReadDir(strings.TrimSuffix(lib, string(os.PathSeparator)))
	if err != nil {
		t.Fatal(err)
	}
	var issueLog, rejectArtifact string
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "sfl-issues-") && strings.HasSuffix(e.Name(), ".log"):
			issueLog = filepath.Join(r.dir, "lib", e.Name())
		case strings.HasPrefix(e.Name(), "sfl_ingest_rejected_") && strings.HasSuffix(e.Name(), ".txt"):
			rejectArtifact = filepath.Join(r.dir, "lib", e.Name())
		}
	}
	if issueLog == "" {
		t.Fatalf("no sfl issue log in library dir:\n%s", stderr)
	}
	issues, err := os.ReadFile(issueLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(issues); !strings.Contains(got, "parse-error") || !strings.Contains(got, "password>64") {
		t.Fatalf("issue log missing emit-gate reason:\n%s", got)
	}
	if rejectArtifact != "" {
		rejects, err := os.ReadFile(rejectArtifact)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(rejects), longPassword) || strings.Contains(string(rejects), "password>64") {
			t.Fatalf("emit-gate rejection was duplicated into ingest artifact:\n%s", rejects)
		}
	}
}

// TestEmitGateRejectStillRecordsHistory pins the 2026-09-30 decision: a
// gate-rejected credential is parse quality, so its source still records in
// history and a rerun skips it.
func TestEmitGateRejectStillRecordsHistory(t *testing.T) {
	r := newExitRun(t)
	r.out = "" // -od replaces -o; the two flags are mutually exclusive
	input := filepath.Join(r.dir, "Passwords.txt")
	body := "URL: a.com\nUSER: u\nPASS: " + strings.Repeat("p", 65) + "\n" +
		"URL: b.com\nUSER: u2\nPASS: short\n"
	if err := os.WriteFile(input, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(r.dir, "lib") + string(os.PathSeparator)
	db := filepath.Join(r.dir, "history.sqlite3")
	_, _, code := r.runSandboxed(t, "-od", lib, "-history", "-history-path", db, input)
	if code != exitcodePartial {
		t.Fatalf("exit = %d, want %d", code, exitcodePartial)
	}
	assertHistoryHit(t, db, candidate.ID, true)
}
