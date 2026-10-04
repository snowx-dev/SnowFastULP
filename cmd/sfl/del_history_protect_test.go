package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// H-03: -del groups sources by top-level child and removes a fully
// successful child recursively — including files that were never validated.
// The effective history database (and its WAL/SHM sidecars) must be on the
// protected list, and protection must survive the recursive directory
// removal, so a database seeded inside an input group cannot be unlinked
// while the connection is open.
func TestRunDelKeepsHistoryDatabaseInsideDeletedGroup(t *testing.T) {
	dir := t.TempDir()
	group := filepath.Join(dir, "in", "group")
	source := filepath.Join(group, "Passwords.txt")
	writeFile(t, source, "URL: example.com\nUSER: user\nPASS: pass\n")
	db := filepath.Join(group, "history.sqlite3")

	cfg := historyRunConfig(filepath.Join(dir, "in"), filepath.Join(dir, "out"), db)
	cfg.DeleteSources = true
	if err := run(cfg); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("history database inside the deleted group was removed: %v", err)
	}
	if _, err := os.Stat(group); err != nil {
		t.Fatalf("group containing the history database was removed: %v", err)
	}
}

// Direct guard on the grouping logic: a protected path nested inside a
// successful directory group must keep the whole group.
func TestDeleteParsedSourcesKeepsGroupContainingProtectedPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	group := filepath.Join(root, "victim")
	source := filepath.Join(group, "Passwords.txt")
	writeFile(t, source, "URL: a.com\nUSER: u\nPASS: p\n")
	db := filepath.Join(group, "history.sqlite3")
	writeFile(t, db, "seed")

	candidate, err := history.FingerprintFile(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := []sflog.SourceResult{{Path: source, OK: true, HistoryComplete: true, HistoryCandidate: candidate}}
	removed, err := deleteParsedSources(root, results, []string{db, db + "-wal", db + "-shm"})
	if err != nil {
		t.Fatalf("deleteParsedSources: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("protected group was deleted: %v", removed)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("protected database was removed: %v", err)
	}
}

// coversProtected must be segment-aware: a sibling whose name extends the
// group's name (victim vs victim2) is NOT beneath it, and a nested path IS.
func TestCoversProtectedIsSegmentAware(t *testing.T) {
	group := filepath.Join("root", "victim")
	nested := filepath.Join(group, "sub", "history.sqlite3")
	sibling := filepath.Join("root", "victim2", "history.sqlite3")
	if coversProtected(group, []string{sibling}) {
		t.Fatalf("sibling %q must not be covered by %q", sibling, group)
	}
	if !coversProtected(group, []string{nested}) {
		t.Fatalf("nested %q must be covered by %q", nested, group)
	}
	if coversProtected(group, []string{group}) {
		t.Fatalf("equality is isProtected's job, not coversProtected's")
	}
}
