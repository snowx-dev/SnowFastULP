package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

func TestCommitHistoryRecordsOnlyCompleteMissesAndValidatesHits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missPath := filepath.Join(dir, "miss.txt")
	hitPath := filepath.Join(dir, "hit.txt")
	os.WriteFile(missPath, []byte("miss"), 0o600)
	os.WriteFile(hitPath, []byte("hit"), 0o600)
	miss, _ := history.FingerprintFile(ctx, missPath, nil)
	hit, _ := history.FingerprintFile(ctx, hitPath, nil)
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	results := []sflog.SourceResult{
		{HistoryComplete: true, HistoryCandidate: miss},
		{HistoryComplete: true, HistoryHit: true, HistoryCandidate: hit},
		{HistoryComplete: false, HistoryCandidate: hit},
	}
	if err := commitHistory(ctx, store, results, false, nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.Lookup(ctx, []history.Identity{miss.ID, hit.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[miss.ID]; !ok {
		t.Fatal("complete miss not recorded")
	}
	if _, ok := got[hit.ID]; ok {
		t.Fatal("hit identity was reinserted")
	}
}

func TestCommitHistorySyncsOutputsBeforeRecordAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	output := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("output"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	oldSync := syncHistoryOutputs
	defer func() { syncHistoryOutputs = oldSync }()
	boom := errors.New("sync failed")
	var synced []string
	syncHistoryOutputs = func(paths []string) error {
		synced = append([]string(nil), paths...)
		return boom
	}
	results := []sflog.SourceResult{{HistoryComplete: true, HistoryCandidate: candidate}}
	if err := commitHistory(ctx, store, results, false, nil, output); !errors.Is(err, boom) {
		t.Fatalf("commitHistory error = %v, want sync failure", err)
	}
	if !reflect.DeepEqual(synced, []string{output}) {
		t.Fatalf("synced paths = %#v, want %#v", synced, []string{output})
	}
	hits, err := store.Lookup(ctx, []history.Identity{candidate.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("history recorded despite sync failure: %#v", hits)
	}
}

func TestCommitHistoryChangedCandidateFailsBeforeInsert(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "source.txt")
	os.WriteFile(p, []byte("before"), 0o600)
	candidate, _ := history.FingerprintFile(ctx, p, nil)
	os.WriteFile(p, []byte("changed-size"), 0o600)
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := commitHistory(ctx, store, []sflog.SourceResult{{HistoryComplete: true, HistoryCandidate: candidate}}, false, nil); err == nil {
		t.Fatal("expected validation error")
	}
	got, _ := store.Lookup(ctx, []history.Identity{candidate.ID})
	if len(got) != 0 {
		t.Fatalf("recorded after validation failure: %v", got)
	}
}

func TestCommitHistoryDryRunDoesNothing(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "source.txt")
	os.WriteFile(p, []byte("before"), 0o600)
	candidate, _ := history.FingerprintFile(ctx, p, nil)
	os.Remove(p)
	if err := commitHistory(ctx, nil, []sflog.SourceResult{{HistoryComplete: true, HistoryCandidate: candidate}}, true, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryHistoryArtifactsReachSyncSeam(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	archive := filepath.Join(dir, "sfu_run.txt.zst")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := filepath.Join(dir, "sfu_dedup_idx", filepath.Base(archive)+".idx")
	searchSidecar := searchidx.LibrarySidecarPath(archive)
	for _, path := range []string{idx, searchSidecar} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	candidate, err := history.FingerprintFile(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	oldSync := syncHistoryOutputs
	defer func() { syncHistoryOutputs = oldSync }()
	var synced []string
	syncHistoryOutputs = func(paths []string) error {
		synced = append([]string(nil), paths...)
		return errors.New("stop after sync")
	}
	outputs := ingestHistoryOutputPaths(&ulpengine.Resolved{OutputPaths: []string{archive}})
	_ = commitHistory(ctx, store, []sflog.SourceResult{{HistoryComplete: true, HistoryCandidate: candidate}}, false, nil, outputs...)
	want := []string{archive, idx, searchSidecar}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("synced paths = %#v, want %#v", synced, want)
	}
}

func TestEnvCopyFailurePreventsHistoryRecordSoArchiveRetries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	archive := filepath.Join(dir, "bundle.zip")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(ctx, archive, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	results := []sflog.SourceResult{{
		Path: archive, HistoryComplete: true, HistoryCandidate: candidate,
	}}
	markEnvCopyIssues(results, []sflog.EnvCopyIssue{{Path: archive + "!config/.env"}})
	if err := commitHistory(ctx, store, results, false, nil); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Lookup(ctx, []history.Identity{candidate.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		// preservation disabled 2026-09-30 (user): env-copy failures record
		// like any other completed source.
		t.Fatalf("env-copy failure did not record history: %#v", hits)
	}
}

func TestCommitHistoryValidationProgressShowsValidatingBar(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(source, []byte(strings.Repeat("x", 2<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(ctx, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var output bytes.Buffer
	results := []sflog.SourceResult{{HistoryComplete: true, HistoryCandidate: candidate}}
	if err := commitHistory(ctx, store, results, false, &output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "history · validating") {
		t.Fatalf("validation progress missing bar title: %q", got)
	}
	if !strings.Contains(got, "1/1 file") {
		t.Fatalf("validation progress missing file count: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("progress contains ANSI: %q", got)
	}
	if !strings.HasSuffix(got, "\r") {
		t.Fatalf("progress line was not cleared: %q", got)
	}
}

// a nil progress writer (non-TTY stderr, -no-tui, legacy console) must print
// nothing at all and not panic
func TestCommitHistoryNilProgressWriterIsSilent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, _ := history.FingerprintFile(ctx, source, nil)
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	results := []sflog.SourceResult{{HistoryComplete: true, HistoryCandidate: candidate}}
	if err := commitHistory(ctx, store, results, false, nil); err != nil {
		t.Fatal(err)
	}
}

// historyAllSkipped gates the sfu-parity one-line summary; pin the exact
// boundary so partial-skip or issue-carrying runs keep the recap box.
func TestHistoryAllSkippedPredicate(t *testing.T) {
	pure := sflog.ExtractStats{HistoryChecked: 60, HistorySkipped: 60}
	if !historyAllSkipped(pure) {
		t.Fatal("all-skipped run must qualify for the one-line summary")
	}
	for name, mut := range map[string]func(*sflog.ExtractStats){
		"partial skip":    func(s *sflog.ExtractStats) { s.HistorySkipped = 59 },
		"emitted lines":   func(s *sflog.ExtractStats) { s.Emitted = 1 },
		"parsed lines":    func(s *sflog.ExtractStats) { s.Credentials = 1 },
		"env copies":      func(s *sflog.ExtractStats) { s.EnvCopied = 1 },
		"tdata copies":    func(s *sflog.ExtractStats) { s.EnvDirsCopied = 1 },
		"failed sources":  func(s *sflog.ExtractStats) { s.FailedSources = 1 },
		"recorded issues": func(s *sflog.ExtractStats) { s.Issues = []sflog.Issue{{Path: "x.zip"}} },
		"no checks":       func(s *sflog.ExtractStats) { s.HistoryChecked = 0 },
	} {
		s := pure
		mut(&s)
		if historyAllSkipped(s) {
			t.Errorf("%s must NOT qualify for the one-line summary", name)
		}
	}
}

func TestHistorySkipSummaryLineNoun(t *testing.T) {
	got := stripANSI(historySkipSummaryLine(1))
	if !strings.Contains(got, "✓ history: 1 source already completed · nothing to process") {
		t.Fatalf("singular = %q", got)
	}
	got = stripANSI(historySkipSummaryLine(67))
	if !strings.Contains(got, "✓ history: 67 sources already completed · nothing to process") {
		t.Fatalf("plural = %q", got)
	}
}
