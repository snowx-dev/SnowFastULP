package main

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// requireRunSuccess asserts run() exited clean (nil): every source succeeded
// or was skipped by history. A *runOutcome is the exit-code policy failing the
// run; anything else is a hard error.
func requireRunSuccess(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var outcome *runOutcome
	if errors.As(err, &outcome) {
		t.Fatalf("run was not clean: %s", outcome.msg)
	}
	t.Fatal(err)
}

// requireRunOutcome asserts run() completed with the expected exit-code policy
// outcome (internal/exitcode): the tests here exercise failing sources whose
// retention -del must guarantee, so the run returns a *runOutcome by design.
func requireRunOutcome(t *testing.T, err error, wantCode int) {
	t.Helper()
	var outcome *runOutcome
	if err == nil {
		t.Fatalf("run exited clean, want runOutcome %d", wantCode)
	}
	if !errors.As(err, &outcome) {
		t.Fatal(err)
	}
	if outcome.code != wantCode {
		t.Fatalf("runOutcome = %d (%s), want %d", outcome.code, outcome.msg, wantCode)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunDeletesParsedTopLevelSubfolders(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	victimA := filepath.Join(input, "victimA")
	victimB := filepath.Join(input, "victimB")
	writeFile(t, filepath.Join(victimA, "All Passwords.txt"), "URL: a.com\nUSER: u\nPASS: p\n")
	writeFile(t, filepath.Join(victimB, "Passwords.txt"), "URL: b.com\nUSER: u2\nPASS: p2\n")
	outDir := filepath.Join(dir, "out")

	runErr := run(runConfig{
		Input: input, OutputDir: outDir, Workers: 2, NoTUI: true, DeleteSources: true,
		RunStamp: "20260626_211000",
	})
	requireRunSuccess(t, runErr)

	if _, err := os.Stat(victimA); !os.IsNotExist(err) {
		t.Fatalf("victimA should be deleted: %v", err)
	}
	if _, err := os.Stat(victimB); !os.IsNotExist(err) {
		t.Fatalf("victimB should be deleted: %v", err)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("input root must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "sfl_20260626_211000.txt")); err != nil {
		t.Fatalf("output must survive: %v", err)
	}
}

func TestDeleteParsedSourcesKeepsReplacementAfterDirectoryStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	group := filepath.Join(root, "victim")
	source := filepath.Join(group, "Passwords.txt")
	writeFile(t, source, "URL: a.com\nUSER: u\nPASS: p\n")
	candidate, err := history.FingerprintFile(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	results := []sflog.SourceResult{{Path: source, OK: true, HistoryComplete: true, HistoryCandidate: candidate}}
	oldHook := afterHistoryDeleteStage
	defer func() { afterHistoryDeleteStage = oldHook }()
	afterHistoryDeleteStage = func([]history.StagedPath) error {
		return os.MkdirAll(filepath.Join(group, "replacement"), 0o700)
	}
	if _, err := deleteParsedSources(root, results, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(group, "replacement")); err != nil {
		t.Fatalf("replacement group was deleted: %v", err)
	}
}

func TestRunDelDeletesNoULPSources(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	loose := filepath.Join(input, "loose")
	writeFile(t, filepath.Join(loose, "Passwords.txt"), "Browser: Chrome\nProfile: Default\n")

	archivePath := filepath.Join(input, "no-ulp.zip")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("info.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("not a credential file\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	runErr := run(runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"), Workers: 1,
		NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 14, 0, 0, time.UTC),
	})
	requireRunOutcome(t, runErr, 4)

	// 2026-09-30 decision: no-ULP sources record in history and -del deletes
	// every recorded source — a no-ULP read is deterministic, so a rerun
	// would find nothing again.
	if _, err := os.Stat(loose); !os.IsNotExist(err) {
		t.Fatalf("no-ULP loose source must be deleted: %v", err)
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("no-ULP archive must be deleted: %v", err)
	}
}

func TestRunDelRetainsWrongPasswordSevenZip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "encrypted.7z")
	fixture, err := os.ReadFile("../../internal/sflog/testdata/7z-encrypted-info.7z")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}

	runErr := run(runConfig{
		Input: archivePath, OutputDir: filepath.Join(dir, "out"),
		Password: "wrong", Workers: 1, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 15, 0, 0, time.UTC),
	})
	requireRunOutcome(t, runErr, 4)
	// preservation disabled 2026-09-30 (user): -del deletes every discovered source.
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("wrong-password 7z must be deleted: %v", err)
	}
}

func TestRunDelRetainsEnvSourceWhenAsyncCopyFails(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "config.env")
	writeFile(t, input, "AWS_ACCESS_KEY_ID=example\n")
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(outDir, "sfl_20260626_211600_secrets")
	if err := os.WriteFile(secretPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	runErr := run(runConfig{
		Input: input, OutputDir: outDir, Workers: 1, Env: true,
		NoTUI: true, DeleteSources: true,
		RunStamp: "20260626_211600",
	})
	requireRunOutcome(t, runErr, 4)
	// preservation disabled 2026-09-30 (user): -del deletes every discovered source.
	if _, err := os.Stat(input); !os.IsNotExist(err) {
		t.Fatalf("env source with async copy failure must be deleted: %v", err)
	}
}

func TestRunDelRetainsEnvSourceOverCopyCap(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "large.env")
	f, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16<<20 + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	runErr := run(runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"), Workers: 1,
		Env: true, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 17, 0, 0, time.UTC),
	})
	requireRunOutcome(t, runErr, 4)
	// preservation disabled 2026-09-30 (user): -del deletes every discovered source.
	if _, err := os.Stat(input); !os.IsNotExist(err) {
		t.Fatalf("over-cap env source must be deleted: %v", err)
	}
}

func TestRunDelRetainsArchiveWithEnvMemberOverCopyCap(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "bundle.zip")
	f, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("URL: https://cap.example.com/login\nUSER: analyst\nPASS: pw\n")); err != nil {
		t.Fatal(err)
	}
	w, err = zw.Create(".env")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 16<<20+1)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	runErr := run(runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"), Workers: 1,
		Env: true, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 18, 0, 0, time.UTC),
	})
	// Exit-code policy (salvage refinement): the Passwords.txt member parsed a
	// usable credential (Emitted 1), so the all-failed run is at worst partial
	// (3) — the env member over the copy cap is the one failure. Output
	// outranks the failed count; the archive itself is still retained.
	requireRunOutcome(t, runErr, 3)
	// preservation disabled 2026-09-30 (user): -del deletes every discovered source.
	if _, err := os.Stat(input); !os.IsNotExist(err) {
		t.Fatalf("archive with over-cap env member must be deleted: %v", err)
	}
}

func TestMarkEnvCopyIssuesMatchesTopLevelSource(t *testing.T) {
	results := []sflog.SourceResult{
		{Path: "/logs/a.zip", HistoryComplete: true},
		{Path: "/logs/a.zip.bak", HistoryComplete: true},
		{Path: "/logs/loose.env", HistoryComplete: true},
	}
	issues := []sflog.EnvCopyIssue{
		{Path: "/logs/a.zip!victim/.env"},
		{Path: "/logs/loose.env"},
	}

	markEnvCopyIssues(results, issues)
	// preservation disabled 2026-09-30 (user): env issues flag the source for
	// reporting but no longer withhold history.
	if !results[0].HadIssue || !results[0].HistoryComplete || !results[2].HadIssue || !results[2].HistoryComplete {
		t.Fatalf("results = %+v, expected matching sources to be marked but complete", results)
	}
	if results[1].HadIssue || !results[1].HistoryComplete {
		t.Fatalf("result with only a shared prefix was modified: %+v", results[1])
	}
}

func TestRunDelRetainsFailedArchiveButDeletesGoodSubfolder(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	victim := filepath.Join(input, "victim")
	writeFile(t, filepath.Join(victim, "Passwords.txt"), "URL: a.com\nUSER: u\nPASS: p\n")
	badZip := filepath.Join(input, "locked.zip")
	writeEncryptedRunZip(t, badZip, "ice", "victim/Passwords.txt", "URL: z.com\nUSER: u\nPASS: p\n")

	pwPath := filepath.Join(dir, "pw.txt")
	writeFile(t, pwPath, "wrong\n")
	outDir := filepath.Join(dir, "out")

	runErr := run(runConfig{
		Input: input, OutputDir: outDir, Password: pwPath, Workers: 2, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 11, 0, 0, time.UTC),
	})
	requireRunOutcome(t, runErr, 3)

	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("good victim should be deleted: %v", err)
	}
	if _, err := os.Stat(badZip); !os.IsNotExist(err) {
		t.Fatalf("bad-password archive must be deleted: %v", err)
	}
}

// A nested archive whose password is missing fails in isolation: the outer
// archive parses OK but is flagged HadIssue, so -del must retain it rather than
// discard the un-cracked inner data.
func TestRunDelRetainsArchiveWithUncrackedNested(t *testing.T) {
	dir := t.TempDir()

	innerPath := filepath.Join(dir, "inner.zip")
	writeEncryptedRunZip(t, innerPath, "ice", "victim/Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")
	innerBytes, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(innerPath); err != nil {
		t.Fatal(err)
	}

	outer := filepath.Join(dir, "outer.zip")
	f, err := os.Create(outer)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("v.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(innerBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	pwPath := filepath.Join(dir, "pw.txt")
	writeFile(t, pwPath, "wrong\n") // lacks "ice"
	outDir := filepath.Join(dir, "out")

	runErr := run(runConfig{
		Input: outer, OutputDir: outDir, Password: pwPath, Workers: 1, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 13, 0, 0, time.UTC),
	})
	requireRunOutcome(t, runErr, 4)

	// preservation disabled 2026-09-30 (user): -del deletes every discovered source.
	if _, err := os.Stat(outer); !os.IsNotExist(err) {
		t.Fatalf("outer archive with an uncracked nested member must be deleted: %v", err)
	}
}

func TestRunDelRemovesSingleArchiveInput(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "logs.zip")
	writeEncryptedRunZip(t, archivePath, "ice", "victim/Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")
	pwPath := filepath.Join(dir, "pw.txt")
	writeFile(t, pwPath, "ice\n")
	outDir := filepath.Join(dir, "out")

	runErr := run(runConfig{
		Input: archivePath, OutputDir: outDir, Password: pwPath, Workers: 1, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 12, 0, 0, time.UTC),
	})
	requireRunSuccess(t, runErr)

	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("single archive input should be deleted: %v", err)
	}
}

// A byte-identical duplicate env file is a successful copy outcome: no issue
// is recorded, the deduped source stays -del eligible, and only one copy of
// the payload lands in the secrets dir.
func TestRunDelRemovesDedupedEnvSource(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	victimA := filepath.Join(input, "victimA")
	victimB := filepath.Join(input, "victimB")
	writeFile(t, filepath.Join(victimA, "config.env"), "API_KEY=shared-value\n")
	writeFile(t, filepath.Join(victimB, "config.env"), "API_KEY=shared-value\n")
	outDir := filepath.Join(dir, "out")

	runErr := run(runConfig{
		Input: input, OutputDir: outDir, Workers: 1, Env: true,
		NoTUI: true, DeleteSources: true,
		RunStamp: "20260920_210000",
	})
	requireRunSuccess(t, runErr)

	secretsDir := filepath.Join(outDir, "sfl_20260920_210000_secrets")
	entries, err := os.ReadDir(secretsDir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "config.env" {
		t.Fatalf("secrets dir should hold exactly one deduped config.env; entries = %v", names)
	}
	for _, victim := range []string{victimA, victimB} {
		if _, err := os.Stat(victim); !os.IsNotExist(err) {
			t.Fatalf("deduped env source %s must still be deleted: %v", victim, err)
		}
	}
}
