package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

func TestPrepareHistoryFiltersHitsWithRealStore(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{
		writeHistoryInput(t, dir, "first.txt", "https://one.example:user:pass\n"),
		writeHistoryInput(t, dir, "second.txt", "https://two.example:user:pass\n"),
		writeHistoryInput(t, dir, "third.txt", "https://three.example:user:pass\n"),
	}

	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := history.FingerprintFile(context.Background(), inputs[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{second.ID}); err != nil {
		t.Fatal(err)
	}

	got, err := prepareHistory(context.Background(), store, inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPending := []string{inputs[0], inputs[2]}
	for i := range wantPending {
		wantPending[i], _ = filepath.Abs(wantPending[i])
	}
	if !reflect.DeepEqual(got.PendingPaths, wantPending) {
		t.Fatalf("pending paths = %#v, want %#v", got.PendingPaths, wantPending)
	}
	if len(got.Candidates) != 3 || len(got.Pending) != 2 || len(got.Hits) != 1 {
		t.Fatalf("candidate counts = all:%d pending:%d hits:%d", len(got.Candidates), len(got.Pending), len(got.Hits))
	}
	if got.HitCount != 1 || got.HitBytes != second.ID.Size {
		t.Fatalf("hit summary = %d/%d, want 1/%d", got.HitCount, got.HitBytes, second.ID.Size)
	}
}

// prepareHistory must fold the live prehash progress into the -json
// counters: byte totals match the fingerprinted size, checked counts every
// fingerprinted source, and skipped carries the database's already-done
// tally once Select resolves.
func TestPrepareHistoryFeedsJSONOutCounters(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{
		writeHistoryInput(t, dir, "first.txt", "https://one.example:user:pass\n"),
		writeHistoryInput(t, dir, "second.txt", "https://two.example:user:pass\n"),
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := history.FingerprintFile(context.Background(), inputs[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{second.ID}); err != nil {
		t.Fatal(err)
	}

	hc := &historyCounters{}
	prepared, err := prepareHistory(context.Background(), store, inputs, hc)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, c := range prepared.Candidates {
		total += c.ID.Size
	}
	if hc.bytesTotal.Load() != total || hc.bytesDone.Load() != total {
		t.Fatalf("bytes counters = done:%d total:%d, want %d/%d", hc.bytesDone.Load(), hc.bytesTotal.Load(), total, total)
	}
	if hc.checked.Load() != int64(len(prepared.Candidates)) {
		t.Fatalf("checked = %d, want %d", hc.checked.Load(), len(prepared.Candidates))
	}
	if hc.skipped.Load() != 1 {
		t.Fatalf("skipped = %d, want the one recorded hit", hc.skipped.Load())
	}
}

func TestPrepareHistoryWithoutStoreTreatsAllInputsAsPending(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{
		writeHistoryInput(t, dir, "one.txt", "one"),
		writeHistoryInput(t, dir, "two.txt", "two"),
	}
	got, err := prepareHistory(context.Background(), nil, inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PendingPaths) != 2 || len(got.Pending) != 2 || len(got.Hits) != 0 {
		t.Fatalf("candidate counts = pending paths:%d pending:%d hits:%d", len(got.PendingPaths), len(got.Pending), len(got.Hits))
	}
}

func TestRunHistoryOnlyAllHitsReportsSuccessWithoutDeleting(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "hit.txt", "https://hit.example:user:pass\n")
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	candidate, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{candidate.ID}); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareHistory(context.Background(), store, []string{input}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	deleted, err := runHistoryOnly(context.Background(), prepared, false, false, &output)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %#v, want none", deleted)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("history hit was deleted without -del: %v", err)
	}
	if got := output.String(); got != "✓ history: 1 source already completed · nothing to process\n" {
		t.Fatalf("summary = %q", got)
	}
}

func TestRunHistoryOnlyDeleteValidHits(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{
		writeHistoryInput(t, dir, "one.txt", "one"),
		writeHistoryInput(t, dir, "two.txt", "two"),
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var ids []history.Identity
	for _, input := range inputs {
		candidate, err := history.FingerprintFile(context.Background(), input, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, candidate.ID)
	}
	if err := store.Record(context.Background(), ids); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareHistory(context.Background(), store, inputs, nil)
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := runHistoryOnly(context.Background(), prepared, false, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted = %#v, want both inputs", deleted)
	}
	for _, input := range inputs {
		if _, err := os.Stat(input); !os.IsNotExist(err) {
			t.Fatalf("history hit still exists: %s (%v)", input, err)
		}
	}
}

func TestRunHistoryOnlyChangedHitAbortsBeforeDeletion(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "hit.txt", "before")
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	candidate, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{candidate.ID}); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareHistory(context.Background(), store, []string{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte("after!"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	deleted, err := runHistoryOnly(context.Background(), prepared, false, true, &output)
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("error = %v, want source changed", err)
	}
	if output.Len() != 0 {
		t.Fatalf("failed all-hit run printed success summary: %q", output.String())
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %#v, want none", deleted)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("changed hit was deleted: %v", err)
	}
}

func TestRunHistoryOnlyDeleteKeepsReplacementCreatedAfterStaging(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "hit.txt", "processed")
	candidate, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared := historyPreparation{Candidates: []history.Candidate{candidate}, Hits: []history.Candidate{candidate}}
	oldHook := afterHistoryDeleteStage
	defer func() { afterHistoryDeleteStage = oldHook }()
	afterHistoryDeleteStage = func([]history.StagedPath) error {
		return os.WriteFile(input, []byte("replacement"), 0o600)
	}
	if _, err := runHistoryOnly(context.Background(), prepared, false, true, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "replacement" {
		t.Fatalf("replacement = %q", got)
	}
}

func TestRunHistoryOnlyDryRunNeverDeletesHits(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "hit.txt", "dry-run")
	candidate, err := history.FingerprintFile(context.Background(), input, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared := historyPreparation{
		Candidates: []history.Candidate{candidate},
		Hits:       []history.Candidate{candidate},
		HitCount:   1,
	}
	deleted, err := runHistoryOnly(context.Background(), prepared, true, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %#v, want none", deleted)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("dry-run deleted history hit: %v", err)
	}
}

func TestCommitHistoryRecordsPendingAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "pending.txt", "pending")
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := prepareHistory(context.Background(), store, []string{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitHistory(context.Background(), store, prepared, false, nil); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Lookup(context.Background(), []history.Identity{prepared.Pending[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hits[prepared.Pending[0].ID]; !ok {
		t.Fatal("pending identity was not recorded")
	}
}

func TestCommitHistorySyncsOutputsBeforeRecordAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "pending.txt", "pending")
	output := writeHistoryInput(t, dir, "output.txt.zst", "durable")
	idx := filepath.Join(dir, "sfu_dedup_idx", filepath.Base(output)+".idx")
	searchSidecar := searchidx.LibrarySidecarPath(output)
	for _, path := range []string{idx, searchSidecar} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := prepareHistory(context.Background(), store, []string{input}, nil)
	if err != nil {
		t.Fatal(err)
	}

	oldSync := syncHistoryOutputs
	defer func() { syncHistoryOutputs = oldSync }()
	boom := errors.New("sync failed")
	var synced []string
	syncHistoryOutputs = func(paths []string) error {
		synced = append([]string(nil), paths...)
		return boom
	}
	if err := commitHistory(context.Background(), store, prepared, false, nil, output); !errors.Is(err, boom) {
		t.Fatalf("commitHistory error = %v, want sync failure", err)
	}
	wantSynced := []string{output, idx, searchSidecar}
	if !reflect.DeepEqual(synced, wantSynced) {
		t.Fatalf("synced paths = %#v, want %#v", synced, wantSynced)
	}
	hits, err := store.Lookup(context.Background(), []history.Identity{prepared.Pending[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("history recorded despite sync failure: %#v", hits)
	}
}

func TestCommitHistoryChangedSourceFailsBeforeAnyRecord(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{
		writeHistoryInput(t, dir, "one.txt", "one"),
		writeHistoryInput(t, dir, "two.txt", "two"),
	}
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := prepareHistory(context.Background(), store, inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputs[1], []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = commitHistory(context.Background(), store, prepared, false, nil)
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("error = %v, want source changed", err)
	}
	ids := []history.Identity{prepared.Pending[0].ID, prepared.Pending[1].ID}
	hits, err := store.Lookup(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("recorded identities after failed validation: %#v", hits)
	}
}

func TestCommitHistoryCancelDuringValidation(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "pending.txt", "pending")
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := prepareHistory(context.Background(), store, []string{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = commitHistory(ctx, store, prepared, false, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("commitHistory error = %v, want context.Canceled", err)
	}
	hits, err := store.Lookup(context.Background(), []history.Identity{prepared.Pending[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("canceled run recorded identity: %#v", hits)
	}
}

func TestCommitHistoryValidationProgressShowsValidatingBar(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "pending.txt", strings.Repeat("x", 2<<20))
	store, err := history.Open(filepath.Join(dir, "history.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	prepared, err := prepareHistory(context.Background(), store, []string{input}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := commitHistory(context.Background(), store, prepared, false, &output); err != nil {
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

func TestRunHistoryOnlyDeleteCancelAfterStageRestoresSources(t *testing.T) {
	dir := t.TempDir()
	inputs := []string{
		writeHistoryInput(t, dir, "one.txt", "one"),
		writeHistoryInput(t, dir, "two.txt", "two"),
	}
	var candidates []history.Candidate
	for _, input := range inputs {
		candidate, err := history.FingerprintFile(context.Background(), input, nil)
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate)
	}
	prepared := historyPreparation{Candidates: candidates, Hits: candidates, HitCount: len(candidates)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldHook := afterHistoryDeleteStage
	defer func() { afterHistoryDeleteStage = oldHook }()
	afterHistoryDeleteStage = func([]history.StagedPath) error {
		// deterministic cancel point: sources are staged (renamed away) but
		// not yet validated or removed
		cancel()
		return nil
	}
	deleted, err := runHistoryOnly(ctx, prepared, false, true, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %#v, want none", deleted)
	}
	for _, input := range inputs {
		if _, statErr := os.Stat(input); statErr != nil {
			t.Fatalf("canceled deletion lost source %s: %v", input, statErr)
		}
	}
}

// An interrupted -del run must still record deleted-so-far: the exit-130
// path renders the partial-outcome block before the "interrupted" line, on
// both the history-only branch and the full-run path. No deterministic
// black-box trigger exists for signal+deleted>0 — the removal loop does no
// long reads and has no per-removal hook — so the interrupted
// classification is covered at the seam (renderInterruptedDeletionOutcome)
// both exit-130 sites hang off.
func TestRenderInterruptedDeletionOutcomeOnInterruptedClassification(t *testing.T) {
	dir := t.TempDir()
	intended := []string{
		writeHistoryInput(t, dir, "one.txt", "one"),
		writeHistoryInput(t, dir, "two.txt", "two"),
		writeHistoryInput(t, dir, "three.txt", "three"),
	}
	deleted := intended[:2]
	remaining := intended[2:]
	cause := context.Canceled

	var out bytes.Buffer
	renderInterruptedDeletionOutcome(&out, intended, deleted, cause)
	got := out.String()
	for _, want := range []string{
		"sfu: delete inputs: context canceled",
		"deleted before failure (2):",
		"    " + deleted[0],
		"    " + deleted[1],
		"not deleted (1):",
		"    " + remaining[0],
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("interrupted outcome missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "restor") {
		t.Fatalf("interrupted outcome claims a rollback:\n%s", got)
	}

	// Pre-deletion interrupt (nothing removed yet): terse, no outcome block.
	out.Reset()
	renderInterruptedDeletionOutcome(&out, intended, nil, cause)
	if out.Len() != 0 {
		t.Fatalf("pre-deletion interrupt rendered output:\n%s", out.String())
	}
}

// Candidates colliding with run outputs (by canonical path or filesystem
// identity) must be excluded from the -del set before staging, and the
// surviving candidates flattened into deletion roots for reporting.
func TestDeletePartialEligibleCandidatesSkipOutputs(t *testing.T) {
	dir := t.TempDir()
	a := writeHistoryInput(t, dir, "a.txt", "a")
	b := writeHistoryInput(t, dir, "b.txt", "b")
	out := writeHistoryInput(t, dir, "sfu_out.txt.zst", "out")
	alias := filepath.Join(dir, "alias.txt")
	if err := os.Link(b, alias); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	aCand, err := history.FingerprintFile(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	bCand, err := history.FingerprintFile(context.Background(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	outCand, err := history.FingerprintFile(context.Background(), out, nil)
	if err != nil {
		t.Fatal(err)
	}
	aliasCand, err := history.FingerprintFile(context.Background(), alias, nil)
	if err != nil {
		t.Fatal(err)
	}

	eligible, roots, err := eligibleDeleteCandidates(
		[]history.Candidate{aCand, outCand, bCand, aliasCand},
		[]string{out},
	)
	if err != nil {
		t.Fatal(err)
	}
	// The output itself is excluded; the alias is a distinct file (hardlink of
	// b, not of the output) and stays in the set.
	if len(eligible) != 3 {
		t.Fatalf("eligible = %d candidates, want 3 (output excluded)", len(eligible))
	}
	wantRoots := []string{a, b, alias}
	if !reflect.DeepEqual(roots, wantRoots) {
		t.Fatalf("roots = %v, want %v", roots, wantRoots)
	}

	// A candidate is dropped wholesale when any of its paths is a filesystem
	// identity match for an output: protecting the output protects b too.
	eligible, roots, err = eligibleDeleteCandidates(
		[]history.Candidate{aCand, bCand, aliasCand},
		[]string{alias},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 1 || !reflect.DeepEqual(roots, []string{a}) {
		t.Fatalf("eligible = %d roots = %v, want only [%s]", len(eligible), roots, a)
	}
}

func TestOpenSFUHistoryDryRunMissingTreatsDatabaseAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "history.sqlite3")
	store, err := openSFUHistory(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if store != nil {
		store.Close()
		t.Fatal("missing read-only history returned a store")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dry-run created history database: %v", err)
	}
}

// an empty source contributes zero bytes but still counts as a file: the
// live counters must reach their final 1/1 state so the inline checking
// bar's files segment and the JSON stream both report it.
func TestPrepareHistoryEmptySourceCountsAsFile(t *testing.T) {
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "empty.txt", "")
	hc := &historyCounters{}
	if _, err := prepareHistory(context.Background(), nil, []string{input}, hc); err != nil {
		t.Fatal(err)
	}
	if hc.checked.Load() != 1 || hc.filesTotal.Load() != 1 {
		t.Fatalf("file counters = checked:%d total:%d, want 1/1", hc.checked.Load(), hc.filesTotal.Load())
	}
	if hc.bytesDone.Load() != 0 || hc.bytesTotal.Load() != 0 {
		t.Fatalf("byte counters = done:%d total:%d, want 0/0 for an empty source", hc.bytesDone.Load(), hc.bytesTotal.Load())
	}
}

func writeHistoryInput(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeRepeatedInput writes line repeated reps times, streaming through a
// bounded buffer: the e2e fixtures run to hundreds of megabytes (1.7 GB for
// the history-prehash shape), and materializing them as one string plus the
// []byte copy writeHistoryInput forces peaked a single -race test process at
// ~12 GB — enough to trip systemd-oomd on 16 GB CI runners (exit 143). The
// file bytes are identical to strings.Repeat(line, reps).
func writeRepeatedInput(t *testing.T, dir, name, line string, reps int64) string {
	t.Helper()
	path, err := writeRepeatedInputErr(dir, name, line, reps)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// writeRepeatedInputErr is the error-returning core of writeRepeatedInput so
// package-level sync.Once fixture builders (no *testing.T in scope) can share
// the streamed writer.
func writeRepeatedInputErr(dir, name, line string, reps int64) (string, error) {
	const perBlock = 1024
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	block := strings.Repeat(line, perBlock)
	fail := func(err error) (string, error) {
		f.Close()
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	for i := reps / perBlock; i > 0; i-- {
		if _, err := w.WriteString(block); err != nil {
			return fail(err)
		}
	}
	if rem := reps % perBlock; rem > 0 {
		if _, err := w.WriteString(strings.Repeat(line, int(rem))); err != nil {
			return fail(err)
		}
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return path, nil
}
