package sflog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

func TestHistoryHitSkipsLooseParsing(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "Passwords.txt")
	if err := os.WriteFile(p, []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Record(context.Background(), []history.Identity{candidate.ID}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	stats, results, err := (&Engine{Workers: 1, History: store}).Run(context.Background(), root, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || stats.FilesScanned != 0 || stats.HistoryChecked != 1 || stats.HistorySkipped != 1 {
		t.Fatalf("out=%q stats=%+v", out.String(), stats)
	}
	if len(results) != 1 {
		t.Fatalf("results=%+v", results)
	}
	r := results[0]
	if !r.OK || r.HadIssue || !r.HistoryComplete || !r.HistoryHit || r.HistoryCandidate.ID != candidate.ID {
		t.Fatalf("result=%+v", r)
	}
}

func TestHistoryMultipartGroupedBeforeFingerprint(t *testing.T) {
	root := t.TempDir()
	whole := filepath.Join(root, "logs.zip")
	writeTestZip(t, whole, map[string]string{"Passwords.txt": "URL: a.com\nUSER: u\nPASS: p\n"})
	data, err := os.ReadFile(whole)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(whole); err != nil {
		t.Fatal(err)
	}
	cut := len(data) / 2
	parts := []string{filepath.Join(root, "logs.zip.001"), filepath.Join(root, "logs.zip.002")}
	if err := os.WriteFile(parts[0], data[:cut], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parts[1], data[cut:], 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintMultipart(context.Background(), "split-parts", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Record(context.Background(), []history.Identity{candidate.ID}); err != nil {
		t.Fatal(err)
	}
	stats, results, err := (&Engine{Workers: 1, History: store}).Run(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.HistoryChecked != 1 || stats.HistorySkipped != 1 || len(results) != 1 {
		t.Fatalf("stats=%+v results=%+v", stats, results)
	}
	if got := len(results[0].HistoryCandidate.Paths); got != 2 {
		t.Fatalf("candidate paths=%d want 2", got)
	}
}

// Incomplete multipart sets (missingFirstVolume) are fingerprinted like any
// other source since 2026-10-01 (user decision): the first run reports the
// missing first volume and records the source; later -history runs skip it
// by content. Identity covers only the parts on disk, so completing the set
// changes the fingerprint and the full set is processed normally.
func TestHistorySkipsMissingFirstVolumeAfterRecord(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, "logs.zip.002")
	if err := os.WriteFile(orphan, []byte("same bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// First run: not in history yet — the orphan is reported, not skipped.
	stats, results, err := (&Engine{Workers: 1, History: store}).Run(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.HistorySkipped != 0 || stats.MissingVolumes != 1 {
		t.Fatalf("stats=%+v, want first-run missing-volume report", stats)
	}
	if len(results) != 1 || results[0].HistoryHit || !results[0].HadIssue || !results[0].HistoryComplete {
		t.Fatalf("results=%+v, want reported-and-recorded incomplete multipart", results)
	}
	// Mirror cmd/sfl commitHistory: HistoryComplete sources record their
	// identity after the run.
	if err := store.Record(context.Background(), []history.Identity{results[0].HistoryCandidate.ID}); err != nil {
		t.Fatal(err)
	}

	// Second run: the recorded orphan is a history hit, so the recap stops
	// showing a phantom archive every run.
	stats2, results2, err := (&Engine{Workers: 1, History: store}).Run(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if stats2.HistoryChecked != 1 || stats2.HistorySkipped != 1 || stats2.MissingVolumes != 0 {
		t.Fatalf("stats=%+v, want recorded orphan skipped by content", stats2)
	}
	if len(results2) != 1 || !results2[0].HistoryHit {
		t.Fatalf("results=%+v, want history hit", results2)
	}
}

func TestHistoryExcludesEnvAndTdataWork(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	items := []workItem{{path: filepath.Join(t.TempDir(), ".env"), kind: kindEnvCopy}, {path: t.TempDir(), kind: kindTelegramCopy}}
	pending, hits, checked, skipped, err := (&Engine{History: store}).selectHistory(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || len(hits) != 0 || checked != 0 || skipped != 0 {
		t.Fatalf("pending=%d hits=%d checked=%d skipped=%d", len(pending), len(hits), checked, skipped)
	}
}

func TestHistoryHitSkipsArchiveExtraction(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "logs.zip")
	writeTestZip(t, p, map[string]string{"Passwords.txt": "URL: a.com\nUSER: u\nPASS: p\n"})
	candidate, err := history.FingerprintFile(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Record(context.Background(), []history.Identity{candidate.ID}); err != nil {
		t.Fatal(err)
	}
	stats, results, err := (&Engine{Workers: 1, History: store}).Run(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchivesScanned != 0 || stats.HistoryChecked != 1 || stats.HistorySkipped != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	if len(results) != 1 || !results[0].HistoryHit || !results[0].IsArchive {
		t.Fatalf("results=%+v", results)
	}
}
