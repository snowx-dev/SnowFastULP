package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/snowx-dev/SnowFastULP/internal/durablefs"
	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/pathident"
	"github.com/snowx-dev/SnowFastULP/internal/tuibar"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

type historyPreparation struct {
	PendingPaths []string
	Candidates   []history.Candidate
	Pending      []history.Candidate
	Hits         []history.Candidate
	CheckedBytes int64
	HitCount     int
	HitBytes     int64
}

// newHistoryBar builds the one-line history progress bar with sfu's palette,
// used by the validating pass, which runs after the monitor has been torn
// down. A nil writer — non-TTY stderr, -no-tui, or a legacy console —
// yields a silent no-op bar, so pipes and logs never receive \r junk.
func newHistoryBar(w io.Writer, phase string) *tuibar.Bar {
	if w == nil {
		return nil
	}
	return tuibar.New(w, tuibar.Style{
		Label: labelStyle,
		Muted: mutedStyle,
		Count: countStyle,
		Byte:  byteStyle,
		Bar:   gradientBar,
		Bytes: humanBytes,
		Rate:  formatRate,
	}, "history · "+phase, termWidthFull)
}

// prepareHistory fingerprints every input and resolves it against the
// history database. The optional counters (nil when -json is off) fold
// the live prehash progress into the -json stream and the inline
// checking bar; they are also nil for tests that only need the preparation
// result. The checking surface itself is that inline bar when the TUI would
// draw; pipes and logs print nothing at all.
func prepareHistory(ctx context.Context, store history.Store, inputs []string, hc *historyCounters) (historyPreparation, error) {
	units := make([]history.FingerprintUnit, len(inputs))
	for i, input := range inputs {
		units[i] = history.FingerprintUnit{Path: input}
	}
	progress := func(filesDone, filesTotal int, bytesDone, bytesTotal int64) {
		if hc != nil {
			hc.record(filesDone, filesTotal, bytesDone, bytesTotal)
		}
	}
	candidates, err := history.FingerprintAll(ctx, units, 0, progress)
	if err != nil {
		return historyPreparation{}, err
	}
	prepared := historyPreparation{Candidates: make([]history.Candidate, 0, len(candidates))}
	for _, candidate := range candidates {
		prepared.Candidates = append(prepared.Candidates, candidate)
		prepared.CheckedBytes += candidate.ID.Size
	}

	if store == nil {
		prepared.Pending = append(prepared.Pending, prepared.Candidates...)
	} else {
		selection, err := history.Select(ctx, store, prepared.Candidates)
		if err != nil {
			return historyPreparation{}, err
		}
		prepared.Pending = selection.Pending
		prepared.Hits = selection.Hits
	}
	if hc != nil {
		hc.setSkipped(len(prepared.Hits))
	}
	prepared.PendingPaths = make([]string, 0, len(prepared.Pending))
	for _, candidate := range prepared.Pending {
		prepared.PendingPaths = append(prepared.PendingPaths, candidate.Paths...)
	}
	prepared.HitCount = len(prepared.Hits)
	for _, candidate := range prepared.Hits {
		prepared.HitBytes += candidate.ID.Size
	}
	return prepared, nil
}

func openSFUHistory(path string, dryRun bool) (history.Store, error) {
	if !dryRun {
		return history.Open(path)
	}
	store, _, err := history.OpenReadOnly(path)
	return store, err
}

var syncHistoryOutputs = durablefs.SyncPaths

func commitHistory(ctx context.Context, store history.Store, prepared historyPreparation, dryRun bool, progressWriter io.Writer, outputPaths ...string) error {
	if dryRun {
		return nil
	}
	if err := syncHistoryOutputs(ulpengine.DurableArtifacts(outputPaths)); err != nil {
		return fmt.Errorf("sync outputs before history record: %w", err)
	}
	// The final revalidation re-fingerprints every input (sampled head+tail,
	// sub-second); keep it visible with the progress bar and cancellable like
	// every other pass so
	// Ctrl-C never hangs here or records a partial run.
	bar := newHistoryBar(progressWriter, "validating")
	defer bar.Finish()
	var total int64
	for _, candidate := range prepared.Candidates {
		total += candidate.ID.Size
	}
	seq := bar.SeqProgress(len(prepared.Candidates), total)
	if err := history.ValidateAllContext(ctx, prepared.Candidates, seq); err != nil {
		return err
	}
	bar.Update(len(prepared.Candidates), len(prepared.Candidates), total, total)
	identities := make([]history.Identity, len(prepared.Pending))
	for i, candidate := range prepared.Pending {
		identities[i] = candidate.ID
	}
	return store.Record(ctx, identities)
}

var afterHistoryDeleteStage func([]history.StagedPath) error

// eligibleDeleteCandidates excludes candidates whose sources collide with the
// run's outputs (by canonical path or filesystem identity) from the -del set
// and flattens the survivors into deletion roots.
func eligibleDeleteCandidates(candidates []history.Candidate, outputs []string) ([]history.Candidate, []string, error) {
	protected := make([]string, 0, len(outputs))
	for _, output := range outputs {
		absolute, err := filepath.Abs(output)
		if err != nil {
			return nil, nil, err
		}
		protected = append(protected, filepath.Clean(absolute))
	}
	eligible := make([]history.Candidate, 0, len(candidates))
	var roots []string
	for _, candidate := range candidates {
		skip := false
		for _, path := range candidate.Paths {
			for _, output := range protected {
				if path == output {
					skip = true
					break
				}
				if same, err := pathident.SameFile(path, output); err == nil && same {
					skip = true
					break
				}
			}
			if skip {
				break
			}
		}
		if skip {
			continue
		}
		eligible = append(eligible, candidate)
		roots = append(roots, candidate.Paths...)
	}
	return eligible, roots, nil
}

func deleteHistoryCandidates(ctx context.Context, candidates []history.Candidate, outputs []string) ([]string, error) {
	eligible, roots, err := eligibleDeleteCandidates(candidates, outputs)
	if err != nil {
		return nil, err
	}
	return history.DeleteStaged(ctx, roots, eligible, afterHistoryDeleteStage)
}

// intendedDeletionRoots mirrors ulpengine.DeleteParsedInputs' skip rules
// (canonical path match plus filesystem identity against the run outputs) to
// compute the exact set -del would remove, for partial-outcome reporting.
func intendedDeletionRoots(inputs, outputs []string) ([]string, error) {
	roots, err := canonicalizePaths(inputs)
	if err != nil {
		return nil, err
	}
	protected, err := canonicalizePaths(outputs)
	if err != nil {
		return nil, err
	}
	var intended []string
	for _, root := range roots {
		if containsString(protected, root) {
			continue
		}
		skipByIdentity := false
		for _, output := range protected {
			if same, err := pathident.SameFile(root, output); err == nil && same {
				skipByIdentity = true
				break
			}
		}
		if skipByIdentity {
			continue
		}
		intended = append(intended, root)
	}
	return intended, nil
}

func canonicalizePaths(paths []string) ([]string, error) {
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		canonical = append(canonical, filepath.Clean(absolute))
	}
	return canonical, nil
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// deleteOutcome is the deterministic partial result of a failed -del run:
// every root the run intended to remove, what was already irreversibly
// deleted, and what remains untouched.
type deleteOutcome struct {
	Intended  []string
	Deleted   []string
	Remaining []string
}

// summarizeDeletion canonicalizes the returned deleted list against the
// intended roots and computes what is left. All lists are deduplicated and
// sorted so the report is deterministic regardless of walk order.
func summarizeDeletion(intended, deleted []string) deleteOutcome {
	o := deleteOutcome{Intended: dedupeSorted(intended), Deleted: dedupeSorted(deleted)}
	deletedSet := make(map[string]struct{}, len(o.Deleted))
	for _, path := range o.Deleted {
		deletedSet[path] = struct{}{}
	}
	for _, path := range o.Intended {
		if _, done := deletedSet[path]; !done {
			o.Remaining = append(o.Remaining, path)
		}
	}
	return o
}

func dedupeSorted(paths []string) []string {
	canonical, err := canonicalizePaths(paths)
	if err != nil {
		// Abs only fails without a working directory; keep the raw paths
		// rather than hiding them from the report.
		canonical = append([]string(nil), paths...)
	}
	seen := make(map[string]struct{}, len(canonical))
	unique := canonical[:0]
	for _, path := range canonical {
		if _, dup := seen[path]; dup {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	sort.Strings(unique)
	return unique
}

// renderDeletionOutcome prints the deterministic partial-outcome block: what
// was irreversibly removed before the failure and what remains. It never
// claims a rollback, because successful removals cannot be undone.
func renderDeletionOutcome(o deleteOutcome, cause error) []string {
	lines := []string{fmt.Sprintf("sfu: delete inputs: %v", cause)}
	if len(o.Deleted) > 0 {
		lines = append(lines, fmt.Sprintf("  deleted before failure (%d):", len(o.Deleted)))
		for _, path := range o.Deleted {
			lines = append(lines, "    "+path)
		}
	}
	if len(o.Remaining) > 0 {
		lines = append(lines, fmt.Sprintf("  not deleted (%d):", len(o.Remaining)))
		for _, path := range o.Remaining {
			lines = append(lines, "    "+path)
		}
	}
	return lines
}

// deletionJSONMessage carries the same counts into the JSON error terminal.
func deletionJSONMessage(o deleteOutcome, cause error) string {
	return fmt.Sprintf("%v (deleted %d of %d source(s) before failure; %d not deleted)",
		cause, len(o.Deleted), len(o.Intended), len(o.Remaining))
}

// renderInterruptedDeletionOutcome reports deleted-so-far to stderr when a
// run is interrupted mid-deletion, so an exit-130 signal path still records
// which irreversible removals happened. With no removals yet it prints
// nothing, keeping a pre-deletion interrupt terse.
func renderInterruptedDeletionOutcome(w io.Writer, intended, deleted []string, cause error) {
	if len(deleted) == 0 {
		return
	}
	outcome := summarizeDeletion(intended, deleted)
	for _, ln := range renderDeletionOutcome(outcome, cause) {
		fmt.Fprintln(w, ln)
	}
}

func runHistoryOnly(ctx context.Context, prepared historyPreparation, dryRun, deleteInputs bool, output io.Writer) ([]string, error) {
	var deleted []string
	if !dryRun && deleteInputs {
		var err error
		deleted, err = deleteHistoryCandidates(ctx, prepared.Candidates, nil)
		if err != nil {
			return deleted, err
		}
	}

	count := len(prepared.Hits)
	noun := "sources"
	if count == 1 {
		noun = "source"
	}
	if output != nil {
		// same style convention as the other stderr summary lines: package
		// lipgloss styles, which degrade to plain text on non-TTY output
		fmt.Fprintf(output, "%s %s %s %s\n",
			okStyle.Render("✓"),
			okStyle.Render("history:"),
			countStyle.Render(fmt.Sprintf("%d", count)),
			mutedStyle.Render(fmt.Sprintf("%s already completed · nothing to process", noun)))
	}
	return deleted, nil
}
