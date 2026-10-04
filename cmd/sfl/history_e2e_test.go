package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	"github.com/snowx-dev/SnowFastULP/internal/history"
)

func TestHistoryE2E_ArchiveRecordsThenSkipsAndDeletesHit(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "logs.zip")
	writeHistoryZip(t, archive, "victim/Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")
	db := filepath.Join(dir, "history.sqlite3")
	if err := run(historyRunConfig(archive, filepath.Join(dir, "out1"), db)); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoryHit(t, db, candidate.ID, true)
	cfg := historyRunConfig(archive, filepath.Join(dir, "out2"), db)
	cfg.DeleteSources = true
	if err := run(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("history hit not deleted: %v", err)
	}
}

func TestHistoryE2E_EnvModeSkipsPreviouslyRecordedArchive(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "env-logs.zip")
	writeHistoryZip(t, archive, "victim/Passwords.txt", "URL: env.example\nUSER: u\nPASS: p\n")
	db := filepath.Join(dir, "history.sqlite3")
	if err := run(historyRunConfig(archive, filepath.Join(dir, "plain-out"), db)); err != nil {
		t.Fatal(err)
	}
	cfg := historyRunConfig(archive, filepath.Join(dir, "env-out"), db)
	cfg.Env = true
	cfg.DeleteSources = true
	if err := run(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("-env history hit was not skipped/deleted: %v", err)
	}
}

// 2026-10-01 (user decision): an incomplete multipart set is fingerprinted on
// the parts that exist, so the gapped head records its identity after the
// first run and later -history runs skip it (no phantom re-report). Identity
// covers only the parts on disk, so completing the set changes the
// fingerprint and the full set is processed normally.
func TestHistoryE2E_IncompleteMultipartRecordsHeadThenSkips(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "broken.zip.001")
	third := filepath.Join(dir, "broken.zip.003")
	if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(third, []byte("third"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), first, nil)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "history.sqlite3")
	// Exit-code policy: both multipart parts fail to parse, so the run exits 4
	// (nothing usable) even though the head's identity is recorded.
	var outcome *runOutcome
	err = run(historyRunConfig(dir, filepath.Join(dir, "out"), db))
	if err == nil || !errors.As(err, &outcome) || outcome.code != exitcode.NothingUsable {
		t.Fatalf("all-failed multipart run err = %v, want runOutcome 4", err)
	}
	assertHistoryHit(t, db, candidate.ID, true)
	// A second run over the same input skips the recorded head.
	cfg := historyRunConfig(dir, filepath.Join(dir, "out2"), db)
	cfg.History = true
	var outcome2 *runOutcome
	err = run(cfg)
	if err != nil && (!errors.As(err, &outcome2) || outcome2.code != exitcode.Clean) {
		t.Fatalf("rerun err = %v, want clean (head skipped)", err)
	}
}

func TestHistoryE2E_NoULPRecordsButWrongPasswordDoesNot(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "history.sqlite3")
	noULP := filepath.Join(dir, "Passwords.txt")
	os.WriteFile(noULP, []byte("Browser: Chrome\n"), 0o600)
	noULPCandidate, _ := history.FingerprintFile(context.Background(), noULP, nil)
	// A no-ULP source records its history candidate but exits 4 per the
	// exit-code policy (nothing usable).
	var outcome *runOutcome
	err := run(historyRunConfig(noULP, filepath.Join(dir, "out-no-ulp"), db))
	if err == nil || !errors.As(err, &outcome) || outcome.code != exitcode.NothingUsable {
		t.Fatalf("no-ULP run err = %v, want runOutcome 4", err)
	}
	assertHistoryHit(t, db, noULPCandidate.ID, true)

	locked := filepath.Join(dir, "locked.zip")
	writeEncryptedRunZip(t, locked, "secret", "Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")
	lockedCandidate, _ := history.FingerprintFile(context.Background(), locked, nil)
	cfg := historyRunConfig(locked, filepath.Join(dir, "out-locked"), db)
	cfg.Password = "wrong"
	// preservation disabled 2026-09-30 (user): a wrong-password source still
	// records; the run exits 4 per the exit-code policy (every source failed).
	err = run(cfg)
	outcome = nil
	if err == nil || !errors.As(err, &outcome) || outcome.code != exitcode.NothingUsable {
		t.Fatalf("wrong-password run err = %v, want runOutcome 4", err)
	}
	assertHistoryHit(t, db, lockedCandidate.ID, true)
}

func TestHistoryE2E_DryRunDoesNotCreateHistoryOrDelete(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "Passwords.txt")
	os.WriteFile(input, []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o600)
	db := filepath.Join(dir, "missing", "history.sqlite3")
	cfg := historyRunConfig(input, "", db)
	cfg.LibraryDir = filepath.Join(dir, "library")
	cfg.DryRun = true
	cfg.DeleteSources = true
	if err := run(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("dry run created db: %v", err)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("dry run deleted source: %v", err)
	}
}

func TestHistoryE2E_RecordFailureKeepsCommittedOutputAndSource(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "Passwords.txt")
	if err := os.WriteFile(input, []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o600); err != nil {
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
	if _, err := db.Exec(`CREATE TRIGGER fail_sfl_history BEFORE INSERT ON history_entries BEGIN SELECT RAISE(ABORT, 'forced sfl failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	cfg := historyRunConfig(input, out, dbPath)
	cfg.DeleteSources = true
	if err := run(cfg); err == nil {
		t.Fatal("expected history record failure")
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("source deleted after record failure: %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) == 0 {
		t.Fatalf("committed output missing: entries=%v err=%v", entries, err)
	}
}

func TestHistoryE2E_GlobalAcrossSFUAndSFL(t *testing.T) {
	bin := buildHistorySFU(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "history.sqlite3")

	fromSFL := filepath.Join(dir, "Passwords.txt")
	if err := os.WriteFile(fromSFL, []byte("https://sfl.example:user:pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(historyRunConfig(fromSFL, filepath.Join(dir, "sfl-out"), db)); err != nil {
		t.Fatal(err)
	}
	out := runHistorySFU(t, bin, dir, fromSFL, filepath.Join(dir, "sfu-after-sfl"), db)
	if !strings.Contains(out, "already completed") {
		t.Fatalf("sfu did not honor sfl history:\n%s", out)
	}

	fromSFU := filepath.Join(dir, "from-sfu.txt")
	// The input must be a recognized sfl source (a credential-named file);
	// a bare login:pass text file was never discovered by sfl.
	seedDir := filepath.Join(dir, "sfu-seed-input")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "Passwords.txt"), []byte("https://sfu.example:user:pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromSFU = seedDir
	runHistorySFU(t, bin, dir, fromSFU, filepath.Join(dir, "sfu-seed"), db)
	sflOut := filepath.Join(dir, "sfl-after-sfu")
	if err := run(historyRunConfig(fromSFU, sflOut, db)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(sflOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("sfl history hit created output: %v", entries)
	}
}

// TestHistoryE2E_IngestRejectStillShowsSummary: parse-quality rejects (a
// 65-char password the shared parser refuses) are reported through the
// issues log and the exit code (3), but since the 2026-09-30 decision they
// no longer withhold history — the source records, the output is committed,
// and the rendered summary stays silent about the state (the issues log
// carries it).
func TestHistoryE2E_IngestRejectStillShowsSummary(t *testing.T) {
	r := newExitRun(t)
	r.out = "" // -od replaces -o; the two flags are mutually exclusive
	input := filepath.Join(r.dir, "Passwords.txt")
	// A 65-char password is extracted by sfl but refused by the shared
	// parser's password>64 cap, so the run carries a parse-quality reject.
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
	stderr, _, code := r.runSandboxed(t, "-od", lib, "-history", "-history-path", db, input)
	if code != exitcodePartial {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodePartial, stderr)
	}
	if !strings.Contains(stderr, "lines in library") {
		t.Fatalf("summary box missing:\n%s", stderr)
	}
	// Final frame header (2026-10-04, sfu harmonization): ✓ COMPLETE above
	// the recap; the quality-reject state lives in exit code + issues log.
	if !strings.Contains(stderr, "✓  COMPLETE") {
		t.Fatalf("summary must carry the ✓ COMPLETE header:\n%s", stderr)
	}
	// Quality rejects record: the source identity authorizes future skips.
	assertHistoryHit(t, db, candidate.ID, true)
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("source must be retained: %v", err)
	}
	entries, err := os.ReadDir(lib)
	if err != nil || len(entries) == 0 {
		t.Fatalf("committed library output missing: entries=%v err=%v", entries, err)
	}
}

func buildHistorySFU(t *testing.T) string {
	t.Helper()
	// The run dir is the test's own t.TempDir(): cleanup is the test
	// process's job via t.Cleanup semantics, never the child's — a
	// force-exited child cannot clean up after itself, and a shared
	// sync.Once build dir would leak into the real temp root.
	bin := filepath.Join(t.TempDir(), "sfu")
	cmd := exec.Command("go", "build", "-o", bin, "../sfu")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfu: %v: %s", err, output)
	}
	return bin
}

func runHistorySFU(t *testing.T, bin, dir, input, out, db string) string {
	t.Helper()
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-config", configPath, "-no-tui", "-no-update-check", "-history", "-history-path", db, "-o", out+string(os.PathSeparator), input)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(dir, "data"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sfu failed: %v\n%s", err, output)
	}
	return string(output)
}

func historyRunConfig(input, out, db string) runConfig {
	return runConfig{Input: input, OutputDir: out, Workers: 1, NoTUI: true, NoUpdateCheck: true,
		RunStamp: "20260921_hist01", History: true, HistoryPath: db}
}

func assertHistoryHit(t *testing.T, db string, id history.Identity, want bool) {
	t.Helper()
	store, exists, err := history.OpenReadOnly(db)
	if err != nil || !exists {
		t.Fatalf("open history: exists=%v err=%v", exists, err)
	}
	defer store.Close()
	hits, err := store.Lookup(context.Background(), []history.Identity{id})
	if err != nil {
		t.Fatal(err)
	}
	_, got := hits[id]
	if got != want {
		t.Fatalf("history hit=%v want %v", got, want)
	}
}

func writeHistoryZip(t *testing.T, path, name, body string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// A pure all-history-skip rerun prints exactly sfu's history-only line — no
// recap box, no "Update available" banner/tagline (parity with sfu's
// history-only path). -odr previews keep the recap box (preview info).
func TestHistoryE2E_AllSkippedOneLineSummary(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "Passwords.txt")
	if err := os.WriteFile(input, []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "history.sqlite3")
	if err := run(historyRunConfig(input, filepath.Join(dir, "out1"), db)); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	runErr := run(historyRunConfig(input, filepath.Join(dir, "out2"), db))
	os.Stderr = old
	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("rerun err = %v, want clean", runErr)
	}
	got := stripANSI(buf.String())
	want := "✓ history: 1 source already completed · nothing to process\n"
	if got != want {
		t.Fatalf("all-skip summary = %q, want exactly %q", got, want)
	}
	for _, bad := range []string{"History", "Input ", "Lines ", "Unique", "Update available"} {
		if strings.Contains(got, bad) {
			t.Errorf("all-skip summary must not contain %q:\n%s", bad, got)
		}
	}
}
