package main

import (
	"context"
	"fmt"
	"io"

	"github.com/snowx-dev/SnowFastULP/internal/durablefs"
	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/tuibar"
)

var syncHistoryOutputs = durablefs.SyncPaths

// newSflHistoryBar builds the one-line history progress bar with sfl's
// palette. A nil writer — non-TTY stderr, -no-tui, or a legacy console —
// yields a silent no-op bar, so pipes and logs never receive \r junk.
//
// Both the checking phase (before the monitor starts) and the validating
// phase (after the monitor has been torn down) render through this bar.
func newSflHistoryBar(w io.Writer, phase string) *tuibar.Bar {
	if w == nil {
		return nil
	}
	return tuibar.New(w, tuibar.Style{
		Label: sflLabelStyle,
		Muted: sflMutedStyle,
		Count: sflCountStyle,
		Byte:  sflByteStyle,
		Bar:   gradientBar,
		Bytes: formatBytes,
		Rate: func(bps float64) string {
			if bps <= 0 {
				return "0B/s"
			}
			return formatBytes(int64(bps)) + "/s"
		},
	}, "history · "+phase, termWidthFull)
}

func commitHistory(ctx context.Context, store history.Store, results []sflog.SourceResult, dryRun bool, progressWriter io.Writer, outputPaths ...string) error {
	if dryRun || store == nil {
		return nil
	}
	if err := syncHistoryOutputs(outputPaths); err != nil {
		return fmt.Errorf("sync outputs before history record: %w", err)
	}
	candidates := make([]history.Candidate, 0, len(results))
	identities := make([]history.Identity, 0, len(results))
	for _, result := range results {
		if len(result.HistoryCandidate.Paths) == 0 {
			continue
		}
		candidates = append(candidates, result.HistoryCandidate)
		if result.HistoryComplete && !result.HistoryHit {
			identities = append(identities, result.HistoryCandidate.ID)
		}
	}
	// The validating pass re-fingerprints every source (sampled head+tail,
	// sub-second); surface it on the one-line
	// bar (the monitor is down by here) and make it cancellable so Ctrl-C
	// never hangs the record.
	var total int64
	for _, candidate := range candidates {
		total += candidate.ID.Size
	}
	bar := newSflHistoryBar(progressWriter, "validating")
	defer bar.Finish()
	seq := bar.SeqProgress(len(candidates), total)
	if err := history.ValidateAllContext(ctx, candidates, seq); err != nil {
		return fmt.Errorf("history validation after output commit: %w", err)
	}
	bar.Update(len(candidates), len(candidates), total, total)
	if err := store.Record(ctx, identities); err != nil {
		return fmt.Errorf("history record after output commit: %w", err)
	}
	return nil
}

func openHistoryStore(path string, dryRun bool) (history.Store, error) {
	if dryRun {
		store, _, err := history.OpenReadOnly(path)
		return store, err
	}
	return history.Open(path)
}
