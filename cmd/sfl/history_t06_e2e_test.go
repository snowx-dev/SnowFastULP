package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

func TestHistoryE2E_T06SandboxedFreshDisabledAllHitPartialAndFailure(t *testing.T) {
	r := newExitRun(t)
	r.out = "" // Exercise explicit -od/-o routes without the harness's default -o.
	input := filepath.Join(r.dir, "inputs")
	if err := os.MkdirAll(input, 0o700); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(input, "clean.zip")
	broken := filepath.Join(input, "broken.zip")
	writeHistoryZip(t, clean, "victim/Passwords.txt", "URL: clean.example\nUSER: user\nPASS: pw\n")
	if err := os.WriteFile(broken, []byte("not a zip archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(r.dir, "history.sqlite3")
	library := filepath.Join(r.dir, "library")
	cleanCandidate, err := history.FingerprintFile(context.Background(), clean, nil)
	if err != nil {
		t.Fatal(err)
	}
	brokenCandidate, err := history.FingerprintFile(context.Background(), broken, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Fresh run: one clean source plus a completed archive-open failure. Both
	// completed identities are recorded by commitHistory after output sync.
	stderr, _, code := r.runSandboxed(t, "-od", library, "-history", "-history-path", db, input)
	if code != exitcodePartial {
		t.Fatalf("fresh run exit=%d, want partial %d\nstderr:\n%s", code, exitcodePartial, stderr)
	}
	assertHistoryHit(t, db, cleanCandidate.ID, true)
	assertHistoryHit(t, db, brokenCandidate.ID, true)

	// Supplying a history path alone does not enable history: the same source
	// is processed again, while the completed rows remain in the database.
	disabledOut := filepath.Join(r.dir, "disabled-out")
	if err := os.MkdirAll(disabledOut, 0o700); err != nil {
		t.Fatal(err)
	}
	stderr, stdout, code := r.runSandboxed(t, "-history-path", db, "-o", disabledOut, input)
	if code != exitcodePartial {
		t.Fatalf("disabled-history run exit=%d, want partial %d\nstderr:\n%s", code, exitcodePartial, stderr)
	}
	files, err := os.ReadDir(disabledOut)
	if err != nil || len(files) == 0 {
		t.Fatalf("disabled-history output directory files=%d err=%v\nstdout:\n%s", len(files), err, stdout)
	}
	assertHistoryHit(t, db, cleanCandidate.ID, true)
	assertHistoryHit(t, db, brokenCandidate.ID, true)

	// All-hit rerun skips both identities.
	stderr, _, code = r.runSandboxed(t, "-od", library, "-history", "-history-path", db, input)
	if code != 0 || !strings.Contains(stderr, "2 sources already completed") {
		t.Fatalf("all-hit rerun exit=%d or summary missing two completed sources\nstderr:\n%s", code, stderr)
	}

	// Partial-hit rerun skips both recorded sources and processes the newly
	// completed source, proving per-source lookup and record behavior.
	newSource := filepath.Join(input, "new.zip")
	writeHistoryZip(t, newSource, "victim/Passwords.txt", "URL: new.example\nUSER: user2\nPASS: pw2\n")
	newCandidate, err := history.FingerprintFile(context.Background(), newSource, nil)
	if err != nil {
		t.Fatal(err)
	}
	stderr, _, code = r.runSandboxed(t, "-od", library, "-history", "-history-path", db, input)
	if code != 0 || !strings.Contains(stderr, "2 skipped") {
		t.Fatalf("partial-hit rerun exit=%d or summary missing two skipped sources\nstderr:\n%s", code, stderr)
	}
	assertHistoryHit(t, db, cleanCandidate.ID, true)
	assertHistoryHit(t, db, brokenCandidate.ID, true)
	assertHistoryHit(t, db, newCandidate.ID, true)
}
