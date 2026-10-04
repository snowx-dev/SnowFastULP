package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// The sfl ingest stage mirrors sfu's -od TUI (see cmd/sfu/od_tui_test.go):
// [1/2 LIBRARY PREP] / [2/2 DEDUPING] phase tags, an OD-style library box
// with Library/Bytes/Throughput/System rows, per-worker rows below the box,
// and bars under the frame. These tests pin the shared copy so sfl and sfu
// cannot drift apart again.

// stripANSI and ingestWorkerBarCol live in tui_test.go.

// The ingest header carries the history-skip count as a muted badge after the
// phase tag, ingestion only (matches sfu's ingestion-header badge); the
// DEDUPING tag stays clean and zero-skip runs show nothing. The EXTRACTING
// header (plain -history runs) carries it too — that is sfl's ingestion
// phase; the COMPLETE header does not.
func TestRenderIngestHistorySkipBadge(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{EnginePhase: int32(ulpengine.PhasePhase0)}
	})
	joined := stripANSI(strings.Join(renderProgress(time.Second, prog, 0, 0, 120, true), "\n"))
	if strings.Contains(joined, "skipped") {
		t.Fatalf("zero-skip ingest header must not show the badge:\n%s", joined)
	}
	prog.SetHistorySkipped(35)
	joined = stripANSI(strings.Join(renderProgress(time.Second, prog, 0, 0, 120, true), "\n"))
	if !strings.Contains(joined, "· 35 skipped") {
		t.Fatalf("ingest header missing the skip badge:\n%s", joined)
	}
	if got := historySkipBadge(prog); !strings.Contains(got, "35 skipped") {
		t.Fatalf("badge helper wrong: %q", got)
	}
	if got := historySkipBadge(nil); got != "" {
		t.Fatalf("nil prog badge must be empty: %q", got)
	}
	// dedup phase: badge gone
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{EnginePhase: int32(ulpengine.PhaseDedup), ShowMerge: true, Unique: 3}
	})
	joined = stripANSI(strings.Join(renderProgress(time.Second, prog, 0, 0, 120, true), "\n"))
	if strings.Contains(joined, "35 skipped") {
		t.Fatalf("dedup header must not carry the skip badge:\n%s", joined)
	}
}

func TestRenderIngestPrepIsODPrimary(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			EnginePhase:     int32(ulpengine.PhasePhase0),
			ODPhase:         int32(ulpengine.ODPhaseRegen),
			ArchivesTotal:   5,
			FilesTotal:      16,
			PartsRegenDone:  3,
			PartsRegenTotal: 16,
			RegenBytesRead:  12 << 30,
			RegenBytesTotal: 80 << 30,
			RegenBPS:        128 << 20,
			LibraryKeys:     3_290_076_168,
		}
	})
	joined := stripANSI(strings.Join(renderProgress(time.Second, prog, 0, 0, 120, true), "\n"))
	for _, want := range []string{
		"[1/2 LIBRARY PREP]",
		"Library",           // box title row
		"one-time re-index", // orientation subtitle
		"5 archives",
		"across 16 files",
		"3 / 16 parts indexed",
		"3.29B lines",
		"Bytes",
		"12.0 GB",
		"80.0 GB",
		"Throughput",
		"128.0 MB/s",
		"System",
		"RAM",
		"CPU",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("prep frame missing %q:\n%s", want, joined)
		}
	}
	// old dialect must not return
	for _, bad := range []string{"INGESTING", "Merge", "already in library", "updating library"} {
		if strings.Contains(joined, bad) {
			t.Errorf("prep frame must not render %q:\n%s", bad, joined)
		}
	}
}

func TestRenderIngestPrepSystemRowInsideBox(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			ODPhase:         int32(ulpengine.ODPhaseRegen),
			RegenBytesTotal: 1 << 30,
		}
	})
	lines := renderProgress(time.Second, prog, 0, 0, 86, true)
	sysIdx := -1
	topIdx, botIdx := -1, -1
	for i, ln := range lines {
		switch {
		case strings.Contains(ln, "System"):
			sysIdx = i
		case strings.Contains(ln, "╭"):
			if topIdx < 0 {
				topIdx = i
			}
		case strings.Contains(ln, "╰"):
			botIdx = i
		}
	}
	if sysIdx < 0 {
		t.Fatalf("prep frame missing System row\nlines:\n%s", strings.Join(lines, "\n"))
	}
	if topIdx < 0 || botIdx < 0 || sysIdx <= topIdx || sysIdx >= botIdx {
		t.Fatalf("System row not inside the OD box (top=%d, sys=%d, bottom=%d)\nlines:\n%s",
			topIdx, sysIdx, botIdx, strings.Join(lines, "\n"))
	}
}

func TestRenderIngestWorkerRowsBelowBox(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			ODPhase:         int32(ulpengine.ODPhaseRegen),
			RegenBytesRead:  1 << 30,
			RegenBytesTotal: 2 << 30,
			Workers: []sflog.IngestWorker{
				{Archive: "/data/lib/sfu_20260101_120000_part04.txt.zst", PartIdx: 4, PartsTotal: 16, BytesDone: 1 << 30, BytesTotal: 2 << 30},
				{Archive: "/data/lib/sfu_20260101_120000_part05.txt.zst", PartIdx: 5, PartsTotal: 16, BytesDone: 512 << 20, BytesTotal: 1 << 30, Committing: true},
			},
		}
	})
	lines := renderProgress(0, prog, 0, 0, 120, true)
	joined := strings.Join(lines, "\n")
	plain := stripANSI(joined)
	for _, want := range []string{
		"20260101_120000_part04", // compacted sfu_ prefix + .txt.zst suffix
		"(4/16)",
		"50%",
		"committing…", // sidecar commit state label
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("worker rows missing %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"workers active", "⠋"} {
		if strings.Contains(plain, bad) {
			t.Errorf("worker rows must not use sfl-only %q (sfu parity):\n%s", bad, joined)
		}
	}
	// worker rows sit BELOW the box (between the bottom border and the bar)
	botIdx, barIdx := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "╰") {
			botIdx = i
		}
		if plain := stripANSI(ln); strings.Contains(plain, "50.0%") || strings.Contains(plain, "100.0%") {
			if strings.Contains(plain, "█") {
				barIdx = i
			}
		}
	}
	for i, ln := range lines {
		if strings.Contains(stripANSI(ln), "20260101_120000_part04") {
			if botIdx < 0 || i <= botIdx {
				t.Fatalf("worker row %d must be below the box bottom (%d):\n%s", i, botIdx, joined)
			}
			if barIdx >= 0 && i >= barIdx {
				t.Fatalf("worker row %d must be above the closing bar (%d):\n%s", i, barIdx, joined)
			}
			break
		}
	}
}

func TestRenderIngestDedupBlockRows(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			EnginePhase:       int32(ulpengine.PhaseDedup),
			Unique:            5_234_567,
			BucketsDone:       41,
			BucketsTotal:      64,
			BucketsBytesRead:  1 << 30,
			BucketsBytesTotal: 2 << 30,
			BusyWorkers:       3,
			DedupWorkers:      8,
			KeysLoaded:        3_290_000_000,
			LibraryKeys:       3_290_076_168,
		}
	})
	joined := stripANSI(strings.Join(renderProgress(0, prog, 0, 0, 86, true), "\n"))
	for _, want := range []string{
		"[2/2 DEDUPING]",
		"Lines",
		"5,234,567 unique so far",
		"Progress",
		"buckets",
		"41 / 64",
		"workers",
		"3 / 8 busy",
		"System",
		"Library",    // matching row
		"matching ·", // sfu's live library match copy
		"loaded",
		"Extract", // solid stage-1 bar label
		"Deduping",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dedup frame missing %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"Hash lookup", "lines/s", "Throughput", "Merge", "added", "already in library", "INGESTING"} {
		if strings.Contains(joined, bad) {
			t.Errorf("dedup frame must not render %q:\n%s", bad, joined)
		}
	}
}

func TestRenderIngestDedupLibraryMatchingInsideBox(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			EnginePhase: int32(ulpengine.PhaseDedup),
			LibraryKeys: 100,
			KeysLoaded:  40,
		}
	})
	lines := renderProgress(0, prog, 0, 0, 86, true)
	sysIdx, topIdx, botIdx, matchIdx := -1, -1, -1, -1
	for i, ln := range lines {
		plain := stripANSI(ln)
		switch {
		case strings.Contains(plain, "System"):
			sysIdx = i
		case strings.Contains(ln, "╭"):
			if topIdx < 0 {
				topIdx = i
			}
		case strings.Contains(ln, "╰"):
			botIdx = i
		case strings.Contains(plain, "matching"):
			matchIdx = i
		}
	}
	if matchIdx < 0 {
		t.Fatalf("dedup frame missing Library matching row:\n%s", strings.Join(lines, "\n"))
	}
	if topIdx < 0 || botIdx < 0 || matchIdx <= topIdx || matchIdx >= botIdx {
		t.Fatalf("matching row not inside the dedup box (top=%d, match=%d, bottom=%d):\n%s",
			topIdx, matchIdx, botIdx, strings.Join(lines, "\n"))
	}
	if sysIdx >= 0 && matchIdx < sysIdx {
		t.Fatalf("matching row must follow the System row (sys=%d, match=%d):\n%s",
			sysIdx, matchIdx, strings.Join(lines, "\n"))
	}
}

func TestSflIngestWorkerRowCapNeverExceedsSfuFloorRules(t *testing.T) {
	if got := sflIngestWorkerRowCap(24, 16, 10); got != 8 {
		t.Errorf("cap(24 rows, 16 workers, 10 non-worker) = %d, want 8 (maxWorkerRowsRendered)", got)
	}
	if got := sflIngestWorkerRowCap(24, 4, 10); got != 4 {
		t.Errorf("cap(24, 4, 10) = %d, want 4 (total workers)", got)
	}
	if got := sflIngestWorkerRowCap(24, 16, 30); got != 0 {
		t.Errorf("cap(24, 16, 30) = %d, want 0 (budget exhausted)", got)
	}
}

// Ingest live frames must keep the frost tagline footer (parity with extract
// and with sfu's OD screens). sflRenderIngest used to omit it entirely.
func TestRenderIngestFramesShowFrostFooter(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	for _, tc := range []struct {
		name string
		iv   sflog.IngestView
	}{
		{
			name: "prep",
			iv: sflog.IngestView{
				EnginePhase:     int32(ulpengine.PhasePhase0),
				ODPhase:         int32(ulpengine.ODPhaseRegen),
				RegenBytesTotal: 1 << 30,
			},
		},
		{
			name: "dedup",
			iv: sflog.IngestView{
				EnginePhase:  int32(ulpengine.PhaseDedup),
				BucketsTotal: 64,
				Unique:       1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iv := tc.iv
			prog := sflog.NewProgress()
			prog.BeginIngest(func() sflog.IngestView { return iv })
			joined := strings.Join(renderProgress(time.Second, prog, 0, 0, 86, true), "\n")
			for _, want := range []string{"sfl is open-source", "snowx.dev"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("%s ingest frame missing frost footer %q:\n%s", tc.name, want, joined)
				}
			}
		})
	}
}

// On a 24-row terminal the prep worker stack must shrink (or drop) so the
// Compose clamp never eats the frost footer — same budget rule as sfu's
// TestRenderPhase0LinesFooterSurvives24Rows.
func TestRenderIngestPrepFooterSurvives24Rows(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	workers := make([]sflog.IngestWorker, 16)
	for i := range workers {
		workers[i] = sflog.IngestWorker{
			Archive:    "/data/lib/sfu_archive_part1.txt.zst",
			PartIdx:    int32(i + 1),
			PartsTotal: 16,
			BytesDone:  int64(i) * (1 << 27),
			BytesTotal: 1 << 30,
		}
	}
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			EnginePhase:     int32(ulpengine.PhasePhase0),
			ODPhase:         int32(ulpengine.ODPhaseRegen),
			ArchivesTotal:   16,
			RegenBytesRead:  1 << 30,
			RegenBytesTotal: 1 << 40,
			Workers:         workers,
		}
	})
	// tests run with stderr NOT a tty, so termHeight() defaults to 24.
	lines := renderProgress(time.Second, prog, 0, 0, 86, true)
	if len(lines) > 23 {
		t.Fatalf("ingest prep frame grew past termHeight-1 (24): %d lines", len(lines))
	}
	tail := strings.Join(lines[len(lines)-3:], "\n")
	if !strings.Contains(tail, "snowx.dev") {
		t.Fatalf("footer dropped on 24-row ingest prep terminal:\n%s", strings.Join(lines, "\n"))
	}
}

// TestIngestHeaderAndLinesGeometry pins the sfl ingest frame geometry: the
// header must show a SINGLE spinner glyph before the phase tag (headerLine
// already prepends indent + spinner, so callers must pass only the tag), and
// the Lines row's value must start at label col 13, aligned with Progress /
// System (sflStatLabel pads "Lines" to 13).
func TestIngestHeaderAndLinesGeometry(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			EnginePhase:  int32(ulpengine.PhaseDedup),
			Unique:       5_234_567,
			BucketsDone:  41,
			BucketsTotal: 64,
			BusyWorkers:  3,
			DedupWorkers: 8,
		}
	})
	lines := renderProgress(0, prog, 0, 0, 86, true)
	plain := make([]string, len(lines))
	for i, ln := range lines {
		plain[i] = stripANSI(ln)
	}
	joined := strings.Join(plain, "\n")

	var header string
	for _, ln := range plain {
		if strings.Contains(ln, "[2/2 DEDUPING]") {
			header = strings.TrimLeft(ln, " ")
			break
		}
	}
	if header == "" {
		t.Fatalf("dedup header line not found in frame:\n%s", joined)
	}
	// tick 0 → lineSpinnerFrames[0] = "|"; headerLine emits glyph + 2 spaces + tag.
	if !strings.HasPrefix(header, "|  [2/2 DEDUPING]") {
		t.Fatalf("header must be a single spinner glyph, two spaces, then the tag; got %q", header)
	}

	var linesRow string
	for _, ln := range plain {
		if strings.Contains(ln, "Lines") && strings.Contains(ln, "unique so far") {
			linesRow = ln
			break
		}
	}
	if linesRow == "" {
		t.Fatalf("Lines row not found in frame:\n%s", joined)
	}
	lIdx := strings.Index(linesRow, "Lines")
	vIdx := strings.Index(linesRow, "5,234,567")
	if lIdx < 0 || vIdx != lIdx+13 {
		t.Fatalf("Lines row: value must start at label col 13 (label padded to 13); got offset %d in %q", vIdx-lIdx, linesRow)
	}

	// Same column as the System row's first value, for cross-row alignment proof.
	for _, ln := range plain {
		if sIdx := strings.Index(ln, "System"); sIdx >= 0 {
			ramIdx := strings.Index(ln, "RAM")
			if ramIdx != sIdx+13 {
				t.Fatalf("System row sanity: \"RAM\" offset %d, want 13, in %q", ramIdx-sIdx, ln)
			}
		}
	}
}
