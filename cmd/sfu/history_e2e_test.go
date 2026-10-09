package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	"github.com/snowx-dev/SnowFastULP/internal/history"
)

var (
	sfuE2EOnce sync.Once
	sfuE2EBin  string
	sfuE2EErr  error
)

func TestHistoryE2E_FirstRunRecordsSecondRunSkips(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://example.com:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	out1 := filepath.Join(dir, "out1")
	out2 := filepath.Join(dir, "out2")
	if err := os.MkdirAll(out1, 0o700); err != nil {
		t.Fatal(err)
	}

	runSFUE2E(t, bin, dir, input, "-o", out1+string(os.PathSeparator), "-history", "-history-path", db)
	if files := regularFiles(t, out1); len(files) == 0 {
		t.Fatal("first run created no output")
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("first run did not create history database: %v", err)
	}

	output := runSFUE2E(t, bin, dir, input, "-o", out2+string(os.PathSeparator), "-history", "-history-path", db)
	if _, err := os.Stat(out2); !os.IsNotExist(err) {
		t.Fatalf("second run created output directory: %v", err)
	}
	if !strings.Contains(output, "✓ history: 1 source already completed · nothing to process") {
		t.Fatalf("second run missing all-hit summary:\n%s", output)
	}
}

func TestHistoryE2E_GlobalAcrossParserModes(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://example.com:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	strictOut := filepath.Join(dir, "strict")
	if err := os.Mkdir(strictOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, input, "-o", strictOut+string(os.PathSeparator), "-history", "-history-path", db)

	for _, tc := range []struct {
		name string
		flag []string
	}{
		{name: "loose", flag: []string{"-loose"}},
		{name: "custom delimiters", flag: []string{"-parse-delims", "|"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			args := append([]string{"-o", out + string(os.PathSeparator), "-history", "-history-path", db}, tc.flag...)
			output := runSFUE2E(t, bin, dir, input, args...)
			if !strings.Contains(output, "already completed · nothing to process") {
				t.Fatalf("parser-mode run was not skipped:\n%s", output)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("parser-mode history hit created output directory: %v", err)
			}
		})
	}
}

func TestHistoryE2E_AllHitStillValidatesCustomParserOptions(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://example.com:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	seedOut := filepath.Join(dir, "seed")
	if err := os.Mkdir(seedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, input, "-o", seedOut+string(os.PathSeparator), "-history", "-history-path", db)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "mutually exclusive", args: []string{"-parse-delims", "|", "-parse-rules", filepath.Join(dir, "rules.txt")}, want: "mutually exclusive"},
		{name: "invalid delimiter", args: []string{"-parse-delims", ":"}, want: "parse delimiters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-history", "-history-path", db}, tc.args...)
			output, err := runSFUE2EResult(t, bin, dir, input, args...)
			if err == nil {
				t.Fatalf("all-hit run accepted invalid parser options:\n%s", output)
			}
			if !strings.Contains(output, tc.want) {
				t.Fatalf("output = %q, want %q", output, tc.want)
			}
		})
	}
}

// The output-dir preflight must run before the history all-hit short-circuit:
// a bad -o (no trailing separator, not an existing directory) must be a usage
// error (exit 2) even when every source is already completed, matching sfl's
// validate-first ordering. Valid flags + all-hit still exits 0 with the skip
// summary and creates no output directory.
func TestHistoryE2E_AllHitStillValidatesOutputDirFlag(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://example.com:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	seedOut := filepath.Join(dir, "seed")
	if err := os.Mkdir(seedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, input, "-o", seedOut+string(os.PathSeparator), "-history", "-history-path", db)

	t.Run("bad -o exits usage", func(t *testing.T) {
		output, err := runSFUE2EResult(t, bin, dir, input,
			"-history", "-history-path", db, "-o", filepath.Join(dir, "missing-out"))
		if err == nil {
			t.Fatalf("all-hit run accepted a file-shaped -o:\n%s", output)
		}
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("sfu failed without exit status: %v\n%s", err, output)
		}
		if ee.ExitCode() != exitcode.Usage {
			t.Fatalf("bad -o with all-hit input: exit = %d, want %d (usage)\n%s",
				ee.ExitCode(), exitcode.Usage, output)
		}
		if !strings.Contains(output, "must be a directory") {
			t.Fatalf("bad -o output missing dir-hint usage wording:\n%s", output)
		}
	})

	t.Run("valid flags still skip", func(t *testing.T) {
		output := runSFUE2E(t, bin, dir, input,
			"-o", seedOut+string(os.PathSeparator), "-history", "-history-path", db)
		if !strings.Contains(output, "✓ history: 1 source already completed · nothing to process") {
			t.Fatalf("valid-flag all-hit run missing skip summary:\n%s", output)
		}
	})
}

// A -del failure partway through must report exactly which sources were
// already deleted and which remain, on stderr and in the JSON error text —
// without requiring the debug log. The subdirectory is made read-only so
// removing b.txt fails after a.txt was already removed.
func TestHistoryE2E_DeletePartialReportsExactPaths(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission bits")
	}
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputs := filepath.Join(dir, "inputs")
	sub := filepath.Join(inputs, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	a := writeHistoryInput(t, inputs, "a.txt", "https://a.example:user:pass\n")
	b := writeHistoryInput(t, sub, "b.txt", "https://b.example:user:pass\n")
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	aAbs, err := filepath.Abs(a)
	if err != nil {
		t.Fatal(err)
	}
	bAbs, err := filepath.Abs(b)
	if err != nil {
		t.Fatal(err)
	}

	// Removing b.txt requires write permission on sub; read-only sub makes
	// that removal fail while a.txt (walked first) is already deleted.
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	output, runErr := runSFUE2EResult(t, bin, dir, inputs, "-o", out+string(os.PathSeparator), "-del")
	defer func() { _ = os.Chmod(sub, 0o700) }()
	if runErr == nil {
		t.Fatalf("del run unexpectedly succeeded:\n%s", output)
	}
	var exitCode int
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	}
	if exitCode != 1 {
		t.Fatalf("exit = %d, want 1\noutput:\n%s", exitCode, output)
	}
	for _, want := range []string{
		"deleted before failure (1):",
		"    " + aAbs,
		"not deleted (1):",
		"    " + bAbs,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("partial-outcome block missing %q:\n%s", want, output)
		}
	}
	// The committed output survives the failed deletion, and the undeleted
	// source is untouched.
	if files := regularFiles(t, out); len(files) == 0 {
		t.Fatal("committed output vanished during failed deletion")
	}
	if _, statErr := os.Stat(b); statErr != nil {
		t.Fatalf("not-deleted source was removed: %v", statErr)
	}
	if _, statErr := os.Stat(a); !os.IsNotExist(statErr) {
		t.Fatalf("deleted-before-failure source is still present: %v", statErr)
	}
}

// The history-only all-hit -del path must report which sources were deleted,
// like the full-run -del flow — the report is the only record of an
// irreversible removal.
func TestHistoryE2E_HistoryOnlyDeleteReportsDeletedPaths(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputsDir := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a := writeHistoryInput(t, inputsDir, "a.txt", "https://a.example:user:pass\n")
	b := writeHistoryInput(t, inputsDir, "b.txt", "https://b.example:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	seedOut := filepath.Join(dir, "seed-out")
	if err := os.Mkdir(seedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, inputsDir, "-o", seedOut+string(os.PathSeparator), "-history", "-history-path", db)

	output := runSFUE2E(t, bin, dir, inputsDir, "-history", "-history-path", db, "-del")
	aAbs, err := filepath.Abs(a)
	if err != nil {
		t.Fatal(err)
	}
	bAbs, err := filepath.Abs(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"history: deleted 2 source(s):",
		"    " + aAbs,
		"    " + bAbs,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("history-only delete report missing %q:\n%s", want, output)
		}
	}
	for _, input := range []string{a, b} {
		if _, statErr := os.Stat(input); !os.IsNotExist(statErr) {
			t.Fatalf("-del retained source %s: %v", input, statErr)
		}
	}
}

// A failed history-only -del must still report the outcome: which sources
// were already deleted and which remain, mirroring the full-run partial-
// outcome block.
func TestHistoryE2E_HistoryOnlyDeleteFailureReportsRemaining(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission bits")
	}
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputsDir := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a := writeHistoryInput(t, inputsDir, "a.txt", "https://a.example:user:pass\n")
	b := writeHistoryInput(t, inputsDir, "b.txt", "https://b.example:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	seedOut := filepath.Join(dir, "seed-out")
	if err := os.Mkdir(seedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, inputsDir, "-o", seedOut+string(os.PathSeparator), "-history", "-history-path", db)

	// Staging the deletion needs a quarantine container inside the input
	// directory; a read-only directory makes staging fail before anything is
	// removed, so both sources must be reported as not deleted.
	if err := os.Chmod(inputsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(inputsDir, 0o700) }()
	output, runErr := runSFUE2EResult(t, bin, dir, inputsDir, "-history", "-history-path", db, "-del")
	if runErr == nil {
		t.Fatalf("history-only del run unexpectedly succeeded:\n%s", output)
	}
	var exitCode int
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	}
	if exitCode != 1 {
		t.Fatalf("exit = %d, want 1\noutput:\n%s", exitCode, output)
	}
	aAbs, err := filepath.Abs(a)
	if err != nil {
		t.Fatal(err)
	}
	bAbs, err := filepath.Abs(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"not deleted (2):",
		"    " + aAbs,
		"    " + bAbs,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("partial-outcome block missing %q:\n%s", want, output)
		}
	}
	for _, input := range []string{a, b} {
		if _, statErr := os.Stat(input); statErr != nil {
			t.Fatalf("source was removed despite failed deletion: %v", statErr)
		}
	}
}

func TestHistoryE2E_DeleteRemovesHitsAndNewlyCompletedSources(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputsDir := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hit := writeHistoryInput(t, inputsDir, "hit.txt", "https://hit.example:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	seedOut := filepath.Join(dir, "seed-out")
	if err := os.Mkdir(seedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, hit, "-o", seedOut+string(os.PathSeparator), "-history", "-history-path", db)

	miss := writeHistoryInput(t, inputsDir, "miss.txt", "https://miss.example:user:pass\n")
	finalOut := filepath.Join(dir, "final-out")
	if err := os.Mkdir(finalOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, inputsDir, "-o", finalOut+string(os.PathSeparator), "-history", "-history-path", db, "-del")
	for _, input := range []string{hit, miss} {
		if _, err := os.Stat(input); !os.IsNotExist(err) {
			t.Fatalf("-del retained source %s: %v", input, err)
		}
	}
	if files := regularFiles(t, finalOut); len(files) == 0 {
		t.Fatal("mixed hit/miss run created no output for the miss")
	}
}

// RR-6.6: uncompressed output written inside the scanned input directory is
// rediscovered by the next run's CollectInputs, so `-del` would delete the
// previous run's output as a "source". The second run must be rejected with a
// usage error before anything is created or removed.
func TestHistoryE2E_DeleteNestedOutputRejected(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputs := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputs, 0o700); err != nil {
		t.Fatal(err)
	}
	input := writeHistoryInput(t, inputs, "input.txt", "https://example.com:user:pass\n")
	out := filepath.Join(inputs, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}

	// First run: uncompressed output lands inside the input tree (no -del).
	runSFUE2E(t, bin, dir, inputs, "-o", out+string(os.PathSeparator))
	previous := regularFiles(t, out)
	if len(previous) == 0 {
		t.Fatal("first run created no output")
	}

	// Second run over the same input directory with -del must exit 2 before
	// touching anything.
	output, runErr := runSFUE2EResult(t, bin, dir, inputs, "-o", out+string(os.PathSeparator), "-del")
	var exitCode int
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	}
	if exitCode != 2 {
		t.Fatalf("exit = %d, want 2\noutput:\n%s", exitCode, output)
	}
	if !strings.Contains(output, "input tree") {
		t.Fatalf("rejection message missing the input-tree explanation:\n%s", output)
	}
	if _, statErr := os.Stat(input); statErr != nil {
		t.Fatalf("source was mutated by the rejected run: %v", statErr)
	}
	after := regularFiles(t, out)
	if len(after) != len(previous) {
		t.Fatalf("rejected run mutated the output directory: %v -> %v", previous, after)
	}
	for _, prev := range previous {
		if _, statErr := os.Stat(prev); statErr != nil {
			t.Fatalf("previous output %s was deleted by the rejected run: %v", prev, statErr)
		}
	}
}

// A symlink alias pointing back into the input tree is the same nesting
// through a different spelling: still rejected with exit 2.
func TestHistoryE2E_DeleteNestedOutputSymlinkAliasRejected(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputs := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputs, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHistoryInput(t, inputs, "input.txt", "https://example.com:user:pass\n")
	out := filepath.Join(inputs, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "out-alias")
	if err := os.Symlink(out, alias); err != nil {
		t.Fatal(err)
	}

	output, runErr := runSFUE2EResult(t, bin, dir, inputs, "-o", alias+string(os.PathSeparator), "-del")
	var exitCode int
	if ee, ok := runErr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	}
	if exitCode != 2 {
		t.Fatalf("exit = %d, want 2\noutput:\n%s", exitCode, output)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected alias run mutated the aliased output directory (%v): %v", err, entries)
	}
}

// Compressed output is not rescanned by CollectInputs, so nesting it under
// the input tree stays allowed.
func TestHistoryE2E_DeleteCompressedOutputUnderInputAllowed(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputs := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputs, 0o700); err != nil {
		t.Fatal(err)
	}
	input := writeHistoryInput(t, inputs, "input.txt", "https://example.com:user:pass\n")
	out := filepath.Join(inputs, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}

	runSFUE2E(t, bin, dir, inputs, "-o", out+string(os.PathSeparator), "-zst", "-del")
	if _, statErr := os.Stat(input); !os.IsNotExist(statErr) {
		t.Fatalf("compressed -del run did not delete its source: %v", statErr)
	}
	if files := regularFiles(t, out); len(files) == 0 {
		t.Fatal("compressed run created no output")
	}
}

// A single-file input cannot rediscover past outputs, so the guard does not
// fire even with the output directory inside the input's own directory.
func TestHistoryE2E_DeleteSingleFileInputOutputUnderInputAllowed(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputs := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputs, 0o700); err != nil {
		t.Fatal(err)
	}
	input := writeHistoryInput(t, inputs, "input.txt", "https://example.com:user:pass\n")
	out := filepath.Join(inputs, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}

	output, runErr := runSFUE2EResult(t, bin, dir, input, "-o", out+string(os.PathSeparator), "-del")
	if runErr != nil {
		t.Fatalf("single-file -del run failed:\n%s", output)
	}
	if _, statErr := os.Stat(input); !os.IsNotExist(statErr) {
		t.Fatalf("single-file -del run did not delete its source: %v", statErr)
	}
	if files := regularFiles(t, out); len(files) == 0 {
		t.Fatal("single-file run created no output")
	}
}

func TestHistoryE2E_DryRunDoesNotCreateRecordWriteOrDelete(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://dry.example:user:pass\n")
	db := filepath.Join(dir, "missing", "history.sqlite3")
	library := filepath.Join(dir, "missing-library")
	runSFUE2E(t, bin, dir, input, "-odr", library, "-history", "-history-path", db, "-del")

	if _, err := os.Stat(input); err != nil {
		t.Fatalf("dry-run deleted input: %v", err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("dry-run created history database: %v", err)
	}
	if _, err := os.Stat(library); !os.IsNotExist(err) {
		t.Fatalf("dry-run created or wrote library: %v", err)
	}
}

func TestHistoryE2E_DryRunConsultsExistingHistoryWithoutRecording(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	inputsDir := filepath.Join(dir, "inputs")
	if err := os.Mkdir(inputsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hit := writeHistoryInput(t, inputsDir, "hit.txt", "https://hit.example:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	seedOut := filepath.Join(dir, "seed")
	if err := os.Mkdir(seedOut, 0o700); err != nil {
		t.Fatal(err)
	}
	runSFUE2E(t, bin, dir, hit, "-o", seedOut+string(os.PathSeparator), "-history", "-history-path", db)

	miss := writeHistoryInput(t, inputsDir, "miss.txt", "https://miss.example:user:pass\n")
	missCandidate, err := history.FingerprintFile(context.Background(), miss, nil)
	if err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(dir, "preview-library")
	output := runSFUE2E(t, bin, dir, inputsDir, "-odr", library, "-history", "-history-path", db, "-del")
	// Input row reports accepted only: 1 file ingested, 1 more skipped.
	if !strings.Contains(output, "1 more skipped") || !strings.Contains(output, "1 file") {
		t.Fatalf("dry-run did not report consulted hit:\n%s", output)
	}
	for _, input := range []string{hit, miss} {
		if _, err := os.Stat(input); err != nil {
			t.Fatalf("dry-run deleted source %s: %v", input, err)
		}
	}
	store, exists, err := history.OpenReadOnly(db)
	if err != nil || !exists {
		t.Fatalf("open existing history: exists=%v err=%v", exists, err)
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), []history.Identity{missCandidate.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := hits[missCandidate.ID]; recorded {
		t.Fatal("dry-run recorded pending source")
	}
}

func TestHistoryE2E_CommitFailureKeepsOutputAndSource(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://commit.example:user:pass\n")
	candidate, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "history.sqlite3")
	store, err := history.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_history_insert
BEFORE INSERT ON history_entries
BEGIN
    SELECT RAISE(ABORT, 'forced commit failure');
END;`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	output, runErr := runSFUE2EResult(t, bin, dir, input, "-o", out+string(os.PathSeparator), "-history", "-history-path", dbPath, "-del")
	if runErr == nil {
		t.Fatalf("run unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(output, "history commit failed") || !strings.Contains(output, "forced commit failure") {
		t.Fatalf("run did not report post-output history failure:\n%s", output)
	}
	// The recoverable state must not suppress the summary: the box renders and
	// the history failure is surfaced inside and after it.
	if !strings.Contains(output, "COMPLETE") {
		t.Fatalf("history commit failure suppressed the summary:\n%s", output)
	}
	if !strings.Contains(output, "History not recorded") || !strings.Contains(output, "inputs were not deleted") {
		t.Fatalf("summary missing history-not-recorded note:\n%s", output)
	}
	if files := regularFiles(t, out); len(files) == 0 {
		t.Fatal("history commit failure removed committed output")
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("history commit failure deleted input: %v", err)
	}
	readStore, exists, err := history.OpenReadOnly(dbPath)
	if err != nil || !exists {
		t.Fatalf("open history after failure: exists=%v err=%v", exists, err)
	}
	defer readStore.Close()
	hits, err := readStore.Lookup(context.Background(), []history.Identity{candidate.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := hits[candidate.ID]; recorded {
		t.Fatal("failed history commit recorded the source")
	}
}

func TestHistoryE2E_ChangedAfterPrehashKeepsOutputAndSourceUnrecorded(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", strings.Repeat("https://changed.example:user:pass\n", 1_000_000))
	original, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "history.sqlite3")
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := sfuE2ECommand(t, bin, dir, input,
		"-o", out+string(os.PathSeparator),
		"-history", "-history-path", dbPath,
		"-del", "-debug", "-no-fast-path", "-workers", "1", "-dedup", "1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		logs, globErr := filepath.Glob(filepath.Join(dir, "sfu-debug-*.log"))
		if globErr != nil {
			cmd.Process.Kill()
			t.Fatal(globErr)
		}
		if len(logs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("debug log did not appear before timeout:\n%s", output.String())
		}
		time.Sleep(time.Millisecond)
	}
	f, err := os.OpenFile(input, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}
	if _, err := f.WriteString("https://late.example:user:pass\n"); err != nil {
		f.Close()
		cmd.Process.Kill()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}

	runErr := cmd.Wait()
	if runErr == nil {
		t.Fatalf("run unexpectedly succeeded:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "source changed") || !strings.Contains(output.String(), "inputs were not deleted") {
		t.Fatalf("run did not report validation failure:\n%s", output.String())
	}
	if files := regularFiles(t, out); len(files) == 0 {
		t.Fatal("validation failure removed committed output")
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("validation failure deleted input: %v", err)
	}
	store, exists, err := history.OpenReadOnly(dbPath)
	if err != nil || !exists {
		t.Fatalf("open history after validation failure: exists=%v err=%v", exists, err)
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), []history.Identity{original.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := hits[original.ID]; recorded {
		t.Fatal("validation failure recorded original identity")
	}
}

func buildSFUE2E(t *testing.T) string {
	t.Helper()
	sfuE2EOnce.Do(func() {
		dir, err := os.MkdirTemp("", "snowfast-sfu-e2e-")
		if err != nil {
			sfuE2EErr = err
			return
		}
		sfuE2EBin = filepath.Join(dir, "sfu")
		cmd := exec.Command("go", "build", "-o", sfuE2EBin, ".")
		if output, err := cmd.CombinedOutput(); err != nil {
			sfuE2EErr = fmt.Errorf("build sfu: %w\n%s", err, output)
		}
	})
	if sfuE2EErr != nil {
		t.Fatal(sfuE2EErr)
	}
	return sfuE2EBin
}

func runSFUE2E(t *testing.T, bin, workdir, input string, args ...string) string {
	t.Helper()
	output, err := runSFUE2EResult(t, bin, workdir, input, args...)
	if err != nil {
		t.Fatalf("sfu failed: %v\n%s", err, output)
	}
	return output
}

func runSFUE2EResult(t *testing.T, bin, workdir, input string, args ...string) (string, error) {
	t.Helper()
	cmd := sfuE2ECommand(t, bin, workdir, input, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func sfuE2ECommand(t *testing.T, bin, workdir, input string, args ...string) *exec.Cmd {
	t.Helper()
	configPath := filepath.Join(workdir, "config.toml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"-config", configPath, "-no-tui", "-no-update-check"}
	argv = append(argv, args...)
	argv = append(argv, input)
	cmd := exec.Command(bin, argv...)
	cmd.Dir = workdir
	dataHome := filepath.Join(workdir, "data-home")
	configHome := filepath.Join(workdir, "config-home")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataHome, "XDG_CONFIG_HOME="+configHome)
	return cmd
}

func regularFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths
}

// -json + -history must stream the checking-history phase from stream
// open: the first line carries phase=checking-history before the engine
// exists, and the summary's history block carries the checked tally.
func TestHistoryE2E_JSONOutFreshRunOpensCheckingHistory(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://example.com:user:pass\n")
	db := filepath.Join(dir, "history.sqlite3")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(dir, "stats.jsonl")

	runSFUE2E(t, bin, dir, input, "-o", out+string(os.PathSeparator), "-history", "-history-path", db, "-json="+jsonl)

	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	first := snaps[0]
	if first["event"] != "start" || first["phase"] != "checking-history" {
		t.Fatalf("first line = %v/%v, want start/checking-history", first["event"], first["phase"])
	}
	if h, ok := first["history"].(map[string]any); !ok || h["enabled"] != true {
		t.Fatalf("start history block = %v, want enabled", first["history"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
	sum, ok := snaps[len(snaps)-1]["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", snaps[len(snaps)-1])
	}
	h, ok := sum["history"].(map[string]any)
	if !ok || h["checked"] != float64(1) {
		t.Fatalf("summary history = %v, want checked=1", sum["history"])
	}
}

// A history-only run (-json + pre-seeded database, every source already
// completed) must emit the full start → updates → done → summary sequence,
// with live byte progress during the prehash. The single large source
// fingerprints for well over one update tick (the prehash runs ~3 GB/s per
// worker), so a tick lands mid-phase with bytes_done advancing.
func TestHistoryE2E_JSONOutHistoryOnlyLiveProgress(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	// The sampled fingerprint reads only head+tail, so the 1.7 GB source
	// no longer spans multiple 210ms ticks on its own; the test-only read
	// pause stretches each sample past the tick instead. 64 MiB is plenty.
	t.Setenv("SNOWFAST_TEST_FP_READ_PAUSE_MS", "150")
	line := "https://site1.example.com:user1:password123456\n"
	reps := (int64(64) << 20) / int64(len(line))
	input := writeRepeatedInput(t, dir, "big.txt", line, reps)

	db := filepath.Join(dir, "history.sqlite3")
	store, err := history.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	cand, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{cand.ID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	size := cand.ID.Size

	jsonl := filepath.Join(dir, "stats.jsonl")
	runSFUE2E(t, bin, dir, input, "-history", "-history-path", db, "-json="+jsonl, "-json-every=210ms")

	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" || snaps[0]["phase"] != "checking-history" {
		t.Fatalf("first line = %v/%v, want start/checking-history", snaps[0]["event"], snaps[0]["phase"])
	}
	var sawLive bool
	prevDone := int64(-1)
	for _, s := range snaps {
		if s["event"] != "update" || s["phase"] != "checking-history" {
			continue
		}
		var done int64
		if h, ok := s["history"].(map[string]any); !ok {
			t.Fatalf("checking-history update without history block: %v", s)
		} else if h["bytes_total"] != float64(size) {
			t.Fatalf("history update = %v, want bytes_total=%d", s, size)
		} else if doneAny, doneOk := h["bytes_done"].(float64); !doneOk || doneAny == 0 {
			// Ticks fired before the first sampled read landed carry no
			// bytes yet (omitempty); they are updates, just not live ones.
			continue
		} else {
			done = int64(doneAny)
		}
		// Live progress invariant: bytes_done never rewinds across observed
		// ticks, however few or many the scheduler lets through under load.
		if prevDone >= 0 && done < prevDone {
			t.Fatalf("checking-history bytes_done rewound: %d after %d", done, prevDone)
		}
		prevDone = done
		sawLive = true
	}
	if !sawLive {
		t.Fatalf("no update with live checking-history bytes: %d lines", len(snaps))
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
	sum, ok := snaps[len(snaps)-1]["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", snaps[len(snaps)-1])
	}
	h, ok := sum["history"].(map[string]any)
	if !ok || h["checked"] != float64(1) || h["skipped"] != float64(1) {
		t.Fatalf("summary history = %v, want checked=1 skipped=1", sum["history"])
	}
	if _, ok := sum["lines"]; ok {
		t.Fatalf("history-only summary must not carry engine lines: %v", sum)
	}
}

// D2: with -json-every far above the 200ms floor, phase transitions must
// still reach the stream — the checking-history start and each phase change
// emit immediately instead of waiting out the interval, so a history phase
// that finishes under the floor is never invisible.
func TestHistoryE2E_JSONOutPhaseTransitionsAboveFloor(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	// A large enough input that the extract/ingest phases outlast one poll
	// window (a 1-line run finishes inside 20ms and shows no transition).
	input := writeHistoryInput(t, dir, "input.txt",
		strings.Repeat("https://example.com:user:pass\n", 400_000))
	db := filepath.Join(dir, "history.sqlite3")
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(dir, "stats.jsonl")

	runSFUE2E(t, bin, dir, input, "-o", out+string(os.PathSeparator),
		"-history", "-history-path", db, "-json="+jsonl, "-json-every=10s")

	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" || snaps[0]["phase"] != "checking-history" {
		t.Fatalf("first line = %v/%v, want start/checking-history", snaps[0]["event"], snaps[0]["phase"])
	}
	// Distinct phases seen across start + updates. With every=10s and a run
	// that finishes in well under one interval, only phase-transition emits
	// can add lines between start and the terminal.
	phases := []string{}
	for _, s := range snaps {
		if s["event"] != "start" && s["event"] != "update" {
			continue
		}
		ph, _ := s["phase"].(string)
		if ph == "" {
			continue
		}
		if len(phases) == 0 || phases[len(phases)-1] != ph {
			phases = append(phases, ph)
		}
	}
	if len(phases) < 2 {
		t.Fatalf("phases = %v, want checking-history followed by a later phase above the floor", phases)
	}
	if phases[0] != "checking-history" {
		t.Fatalf("first phase = %q, want checking-history", phases[0])
	}
	for _, ph := range phases[1:] {
		if ph == "checking-history" {
			t.Fatalf("phases = %v, phase transitions must move forward past checking-history", phases)
		}
	}
}
