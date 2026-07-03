package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestRenderFullIncludesFooter(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.ArchivesTotal.Store(4)
	m.IndexBytesTotal.Store(1 << 30)

	joined := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, ""), "\n")
	for _, want := range []string{"sfs is open-source", "https://snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing footer %q in:\n%s", want, joined)
		}
	}
}

func TestRenderFinalSummaryIncludesFooter(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(42)
	m.ArchivesDone.Store(10)
	m.ArchivesTotal.Store(36)
	m.ChunksDone.Store(100)
	m.ChunksTotal.Store(313)
	m.BytesScanned.Store(512 << 20)
	m.BytesScannedTotal.Store(2 << 30)
	joined := strings.Join(renderFinalSummary(time.Now().Add(-time.Minute), m, "", "", nil), "\n")
	for _, want := range []string{"sfs is open-source", "https://snowx.dev", "COMPLETE", "42", "10", "36", "100", "313"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestRenderFullIndexFrameScanLabel(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.ArchivesTotal.Store(2)
	m.IndexBytesTotal.Store(1 << 30)
	m.BeginFrameScan()

	joined := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, ""), "\n")
	if !strings.Contains(joined, "INDEXING · frame scan") {
		t.Fatalf("missing frame scan label:\n%s", joined)
	}
}

func TestIndexPercentStartsAtZero(t *testing.T) {
	m := &search.Metrics{}
	m.ArchivesTotal.Store(32)
	m.IndexArchivesActive.Store(8)
	m.IndexBytesTotal.Store(1000)

	if pct := indexPercent(m); pct != 0 {
		t.Fatalf("indexPercent = %v, want 0", pct)
	}

	m.ArchivesIndexed.Store(4)
	want := 0.125
	if pct := indexPercent(m); pct != want {
		t.Fatalf("indexPercent = %v, want %v", pct, want)
	}
}

func TestRenderFullSearchStartsWithZeroProgressBars(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	m.ArchivesTotal.Store(36)
	m.ChunksTotal.Store(313)
	m.BytesScannedTotal.Store(100 << 30)

	joined := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, ""), "\n")
	if strings.Contains(joined, "100.0%") {
		t.Fatalf("search should not open with a 100%% bar:\n%s", joined)
	}
}

func TestRenderFullSearchShowsLabeledProgressBars(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	m.ArchivesTotal.Store(36)
	m.ChunksTotal.Store(313)
	m.BytesScannedTotal.Store(100)
	m.BytesScanned.Store(29)
	m.BytesChunkDone.Store(3)

	joined := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, ""), "\n")
	for _, want := range []string{"Chunks", "Scanned", "█", "░", "%"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	for _, ln := range strings.Split(joined, "\n") {
		trim := strings.TrimSpace(ln)
		if strings.Contains(trim, "Chunks /") || strings.Contains(trim, "Scanned /") {
			t.Fatalf("stat counters must stay inside the frame, not on bar rows: %q\n%s", ln, joined)
		}
	}
}

func TestRenderFullSearchShowsByteWeightedChunkProgress(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	m.ArchivesTotal.Store(1)
	m.ChunksTotal.Store(16)
	m.ChunksDone.Store(0)
	m.BytesScannedTotal.Store(100)
	m.BytesScanned.Store(50)

	joined := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, ""), "\n")
	if !strings.Contains(joined, "Chunks") || !strings.Contains(joined, "8.0 / 16") {
		t.Fatalf("missing byte-weighted chunk progress:\n%s", joined)
	}
}

func TestRenderFullHidesOutputPathDuringRun(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	m.ArchivesTotal.Store(1)
	m.ChunksTotal.Store(1)

	path := filepath.Join(t.TempDir(), "hits", "gleeden.txt")
	joined := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, ""), "\n")
	if strings.Contains(joined, path) {
		t.Fatalf("live TUI should not show output path during run:\n%s", joined)
	}
}

func TestRenderFinalSummaryShowsOutputOutsideFrame(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(1)
	path := filepath.Join(t.TempDir(), "hits", "gleeden.txt")
	joined := strings.Join(renderFinalSummary(time.Now(), m, path, "", nil), "\n")
	closeIdx := strings.Index(joined, "╰")
	pathIdx := strings.Index(joined, path)
	if closeIdx < 0 || pathIdx < closeIdx {
		t.Fatalf("output path should be below the COMPLETE frame:\n%s", joined)
	}
	if !strings.Contains(joined, "┃") {
		t.Fatalf("missing sfu-style output gutter:\n%s", joined)
	}
}

// bars start at col 4, aligned w/ stat rows above; balanced layout leaves a
// matching 4-col right margin (same as sfu).
func TestRenderFullBarsAreBalanced(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	m.ArchivesTotal.Store(36)
	m.ChunksTotal.Store(313)
	m.BytesScannedTotal.Store(100)
	m.BytesScanned.Store(29)
	m.BytesChunkDone.Store(3)

	lines := renderFullAt(time.Now(), time.Now(), m, uiRates{}, "", 80)
	var bar1, bar2 string
	for _, ln := range lines {
		if !strings.HasPrefix(ln, indentStr) {
			continue
		}
		if strings.Contains(ln, "█") || (strings.Contains(ln, "░") && (strings.Contains(ln, "%") || strings.Contains(ln, "----"))) {
			if bar1 == "" {
				bar1 = ln
			} else if bar2 == "" {
				bar2 = ln
				break
			}
		}
	}
	if bar1 == "" || bar2 == "" {
		t.Fatalf("could not find two progress bars in %d lines", len(lines))
	}
	if w := tuiVisibleWidth(bar1); w != 76 {
		t.Errorf("bar1 visible width = %d, want 76", w)
	}
	if w := tuiVisibleWidth(bar1); w > 80-leftPad {
		t.Errorf("bar1 right edge not balanced: width %d exceeds %d", w, 80-leftPad)
	}
}
