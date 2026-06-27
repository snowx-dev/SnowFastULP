package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// worker row count scales w/ term height. floor = maxWorkerRowsRendered,
// ceiling = totalWorkers, expands in the middle
func TestWorkerRowCapAdapts(t *testing.T) {
	tests := []struct {
		name         string
		termHeight   int
		totalWorkers int
		want         int
	}{
		{"tiny term keeps floor", 10, 16, maxWorkerRowsRendered},
		{"default xterm (24-18=6, below floor) keeps floor", 24, 16, maxWorkerRowsRendered},
		{"26-row term right at floor", 26, 16, maxWorkerRowsRendered},
		{"30-row term expands (30-18=12)", 30, 16, 12},
		{"40-row term capped by totalWorkers (40-18=22, clamped to 16)", 40, 16, 16},
		{"very tall term capped by totalWorkers", 100, 16, 16},
		{"few workers caps below floor", 24, 3, 3},
		{"zero workers", 40, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workerRowCap(tt.termHeight, tt.totalWorkers); got != tt.want {
				t.Errorf("workerRowCap(%d, %d) = %d, want %d",
					tt.termHeight, tt.totalWorkers, got, tt.want)
			}
		})
	}
}

// never regress below maxWorkerRowsRendered on VT100-default 24 rows
func TestWorkerRowCapPreservesFloor(t *testing.T) {
	for h := 5; h <= 24; h++ {
		if got := workerRowCap(h, 32); got != maxWorkerRowsRendered {
			t.Errorf("termHeight=%d: cap=%d, want floor=%d", h, got, maxWorkerRowsRendered)
		}
	}
}

// parts denominator (ticks) wins over archives when both populated
func TestLibraryRowShowsPartsProgressDuringRegen(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.FilesTotal.Store(16)
	m.ArchivesNeedRegen.Store(1)
	m.PartsRegenTotal.Store(16)
	m.PartsRegenDone.Store(7)
	m.RegenBytesTotal.Store(34 * 1024 * 1024 * 1024)
	m.RegenBytesRead.Store(15 * 1024 * 1024 * 1024)

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if !strings.Contains(out, "7 / 16 parts indexed") {
		t.Errorf("missing parts-progress label\nout:\n%s", out)
	}
	// archive-grained label suppressed when parts is populated
	if strings.Contains(out, "0 / 1 indexing") {
		t.Errorf("archive-grained label should be suppressed when parts known\nout:\n%s", out)
	}
}

// partsRegenTotal=0 = legacy archive-grained label kicks in
func TestLibraryRowFallsBackToArchiveProgress(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(3)
	m.ArchivesNeedRegen.Store(3)
	m.ArchivesRegenedDone.Store(1)
	// partsRegenTotal intentionally 0

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if !strings.Contains(out, "1 / 3 indexing") {
		t.Errorf("missing fallback archive-grained label\nout:\n%s", out)
	}
}

// worker mini-bars must sit outside the gradientBox, indented to match
// the main progress bar
func TestWorkerBarsRenderedOutsideFrame(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.ArchivesNeedRegen.Store(1)
	m.RegenBytesTotal.Store(1 << 30)
	m.RegenBytesRead.Store(1 << 28)
	m.Workers = make([]ulpengine.WorkerStatus, 1)
	name := "sfu_test_part1.txt.zst"
	m.Workers[0].ArchivePath.Store(&name)
	m.Workers[0].PartIdx.Store(1)
	m.Workers[0].PartsTotal.Store(1)
	m.Workers[0].BytesDone.Store(1 << 27)
	m.Workers[0].BytesTotal.Store(1 << 28)

	lines := renderODFrame(m, 0, 100)
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "[1]") {
		t.Fatalf("worker row missing\nout:\n%s", out)
	}

	// worker row must sit between box-bottom border and the main bar
	var workerLineIdx, boxBottomIdx int = -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "[1]") {
			workerLineIdx = i
		}
		// gradientBox bottom border = rounded corner glyph
		if strings.Contains(ln, "╰") || strings.Contains(ln, "╯") {
			boxBottomIdx = i
		}
	}
	if workerLineIdx < 0 || boxBottomIdx < 0 {
		t.Fatalf("could not locate worker line (%d) or box bottom (%d)\nlines:\n%s",
			workerLineIdx, boxBottomIdx, out)
	}
	if workerLineIdx <= boxBottomIdx {
		t.Errorf("worker bar should appear AFTER the box bottom, got worker@%d box-bottom@%d\nlines:\n%s",
			workerLineIdx, boxBottomIdx, out)
	}
}

// bullets fit inside maxInnerWidth = one row
func TestRenderRemovedRowsSingleLineFits(t *testing.T) {
	bullets := []string{
		warnStyle.Render("100") + " " + mutedStyle.Render("rejected"),
		countStyle.Render("50") + " " + mutedStyle.Render("duplicates"),
	}
	rows := renderRemovedRows(bullets, 80)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	plain := stripANSI(rows[0])
	if !strings.Contains(plain, "Removed") || !strings.Contains(plain, "rejected") ||
		!strings.Contains(plain, "duplicates") {
		t.Errorf("single-line missing bullets: %q", plain)
	}
}

// overflow = one bullet per line, indented under label. repros the
// "Removed ... 102M already i…" truncation bug
func TestRenderRemovedRowsMultiLineWhenOverflowing(t *testing.T) {
	bullets := []string{
		warnStyle.Render("55,922,872") + " " + mutedStyle.Render("rejected"),
		countStyle.Render("6,949,904") + " " + mutedStyle.Render("duplicates"),
		countStyle.Render("102,605,832") + " " + mutedStyle.Render("already in library"),
	}
	// inner width ~70 cells, realistic recap frame
	rows := renderRemovedRows(bullets, 70)
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows (one per bullet), got %d:\n%s", len(rows), strings.Join(rows, "\n"))
	}
	if !strings.Contains(stripANSI(rows[0]), "Removed") {
		t.Errorf("first row should carry the 'Removed' label, got %q", stripANSI(rows[0]))
	}
	// continuation rows start w/ label-aligned indent
	for i := 1; i < len(rows); i++ {
		plain := stripANSI(rows[i])
		if strings.Contains(plain, "Removed") {
			t.Errorf("continuation row %d should NOT repeat the label, got %q", i, plain)
		}
		if !strings.HasPrefix(plain, "         ") { // 9 spaces == len("Removed  ")
			t.Errorf("continuation row %d not indented under label: %q", i, plain)
		}
	}
	joined := stripANSI(strings.Join(rows, "\n"))
	for _, want := range []string{"55,922,872", "6,949,904", "102,605,832", "already in library"} {
		if !strings.Contains(joined, want) {
			t.Errorf("multi-line output missing %q\nout:\n%s", want, joined)
		}
	}
}

// zero removals = no rows, no orphan "Removed" label
func TestRenderRemovedRowsEmpty(t *testing.T) {
	if got := renderRemovedRows(nil, 80); len(got) != 0 {
		t.Errorf("empty bullets should produce no rows, got %v", got)
	}
}

// mixed bullet widths must keep indentation stable. guards vs hidden
// padding sneaking into one bullet
func TestRenderRemovedRowsAlignmentStable(t *testing.T) {
	bullets := []string{
		warnStyle.Render("9") + " " + mutedStyle.Render("rejected"), // tiny
		countStyle.Render("999,999,999,999") + " " + mutedStyle.Render("duplicates"),
		countStyle.Render("12,345,678") + " " + mutedStyle.Render("already in library"),
	}
	rows := renderRemovedRows(bullets, 40) // force multi-line
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	// bullet text starts at col 9 on every row (label or indent prefix)
	for i, row := range rows {
		plain := stripANSI(row)
		if len(plain) < 9 {
			t.Errorf("row %d too short: %q", i, plain)
			continue
		}
		// col 9 must be a digit, bullet always starts with count
		c := plain[9]
		if c < '0' || c > '9' {
			t.Errorf("row %d bullet doesn't start at column 9, got %q at col 9 of %q",
				i, c, plain)
		}
	}
}

// bars must start at same column regardless of filename length.
// prev impl let the bar slide = vertical scanning impossible
func TestWorkerRowsBarsAlignAcrossRows(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.ArchivesNeedRegen.Store(1)
	m.RegenBytesTotal.Store(1 << 40)
	m.Workers = make([]ulpengine.WorkerStatus, 3)

	// rows w/ very different name widths
	names := []string{
		"sfu_a_part1.txt.zst",
		"sfu_some_longer_archive_id_part12.txt.zst",
		"sfu_xy_part2.txt.zst",
	}
	for i := range m.Workers {
		n := names[i]
		m.Workers[i].ArchivePath.Store(&n)
		m.Workers[i].PartIdx.Store(int32(i + 1))
		m.Workers[i].PartsTotal.Store(16)
		m.Workers[i].BytesDone.Store(int64(i) * (1 << 28))
		m.Workers[i].BytesTotal.Store(1 << 30)
	}

	lines := renderODFrame(m, 0, 120)
	var rows []string
	for _, ln := range lines {
		if strings.Contains(ln, "[1]") || strings.Contains(ln, "[2]") || strings.Contains(ln, "[3]") {
			rows = append(rows, ln)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 worker rows, got %d:\n%s", len(rows), strings.Join(rows, "\n"))
	}

	// bar starts at first ▆ or ░, must be same col across rows
	cols := make([]int, len(rows))
	for i, ln := range rows {
		cols[i] = visibleColumnOfBar(ln)
	}
	for i := 1; i < len(cols); i++ {
		if cols[i] != cols[0] {
			t.Errorf("bar column misaligned: row[0]=%d row[%d]=%d\nrows:\n%s",
				cols[0], i, cols[i], strings.Join(rows, "\n"))
		}
	}
}

// per-cell truecolor SGR runs, not a flat fg style
func TestWorkerBarHasGradient(t *testing.T) {
	forceTrueColor(t)
	bar := miniGradientBar(1.0, 20, footerGradA, footerGradB)
	// truecolor fg SGR = \x1b[38;2;R;G;Bm
	parts := strings.Split(bar, "\x1b[38;2;")
	if len(parts)-1 < 10 {
		t.Errorf("expected >=10 distinct truecolor SGR runs in a full gradient mini-bar, got %d in %q",
			len(parts)-1, bar)
	}
	// endpoints must differ, that's the gradient
	first := strings.SplitN(parts[1], "m", 2)[0]
	last := strings.SplitN(parts[len(parts)-1], "m", 2)[0]
	if first == last {
		t.Errorf("worker bar gradient endpoints should differ; both = %q", first)
	}
}

// worker palette differs from main phase 1/2. OD = frost-blue,
// main = purple-pink. guards vs accidental palette unification
func TestWorkerBarDistinctFromMainBar(t *testing.T) {
	forceTrueColor(t)
	worker := miniGradientBar(1.0, 8, footerGradA, footerGradB)
	main := gradientBar(1.0, 16) // includes pct suffix

	workerFirst := firstTruecolorSeq(worker)
	mainFirst := firstTruecolorSeq(main)
	if workerFirst == "" || mainFirst == "" {
		t.Skip("non-truecolor profile (non-TTY runner)")
	}
	if workerFirst == mainFirst {
		t.Errorf("worker and main bars share the same start colour (%q); palettes should differ", workerFirst)
	}
}

func firstTruecolorSeq(s string) string {
	parts := strings.Split(s, "\x1b[38;2;")
	if len(parts) < 2 {
		return ""
	}
	return strings.SplitN(parts[1], "m", 2)[0]
}

// visible col of first bar cell (▆ or ░), for alignment checks
func visibleColumnOfBar(line string) int {
	plain := stripANSI(line)
	for i, r := range plain {
		if r == '▆' || r == '░' {
			return i
		}
	}
	return -1
}

// phaseIndex rebrand: "Output index" not "Destination dedup".
// worker rows + byte progress still render
func TestRenderODFrameSwitchesToOutputIndexHeader(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseIndexOwn))
	m.ArchivesTotal.Store(1)
	m.FilesTotal.Store(4)
	m.ArchivesNeedRegen.Store(1)
	m.PartsRegenTotal.Store(4)
	m.PartsRegenDone.Store(2)
	m.RegenBytesTotal.Store(1 << 30)
	m.RegenBytesRead.Store(1 << 28)
	m.Workers = make([]ulpengine.WorkerStatus, 1)
	name := "sfu_run_part1.txt.zst"
	m.Workers[0].ArchivePath.Store(&name)
	m.Workers[0].PartIdx.Store(1)
	m.Workers[0].PartsTotal.Store(1)
	m.Workers[0].BytesDone.Store(1 << 27)
	m.Workers[0].BytesTotal.Store(1 << 28)

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if !strings.Contains(out, "Output index") {
		t.Errorf("missing rebranded header for odPhaseIndexOwn\nout:\n%s", out)
	}
	if strings.Contains(out, "Destination dedup") {
		t.Errorf("stale Destination dedup label leaked into output-index frame\nout:\n%s", out)
	}
	if !strings.Contains(out, "indexing this run's output") {
		t.Errorf("missing odPhaseIndexOwn phaseDesc\nout:\n%s", out)
	}
	if !strings.Contains(out, "2 / 4 parts indexed") {
		t.Errorf("parts counter missing during output indexing\nout:\n%s", out)
	}
	if !strings.Contains(out, "[1]") {
		t.Errorf("worker row missing for output indexing\nout:\n%s", out)
	}
}

// phaseIndex must pull from outputIdxMetrics, not stale odMetrics
func TestRenderIndexLinesUsesOutputIdxMetrics(t *testing.T) {
	// stale phase-0 odMetrics, must not surface
	odM := &ulpengine.ODMetrics{}
	odM.Phase.Store(int32(ulpengine.ODPhaseDone))
	odM.RegenBytesTotal.Store(99 * 1024 * 1024 * 1024)
	odM.RegenBytesRead.Store(99 * 1024 * 1024 * 1024)

	outM := &ulpengine.ODMetrics{}
	outM.Phase.Store(int32(ulpengine.ODPhaseIndexOwn))
	outM.ArchivesTotal.Store(1)
	outM.FilesTotal.Store(4)
	outM.PartsRegenTotal.Store(4)
	outM.PartsRegenDone.Store(1)
	outM.RegenBytesTotal.Store(1 << 30)
	outM.RegenBytesRead.Store(1 << 28)

	r := &ulpengine.Resolved{OdMetrics: odM, OutputIdxMetrics: outM}
	m := &ulpengine.Metrics{}
	out := strings.Join(renderIndexLines(time.Second, m, r, 100, 50, 0, 120), "\n")

	if !strings.Contains(out, "[3/3 INDEXING OUTPUT]") {
		t.Errorf("missing phase-tag header\nout:\n%s", out)
	}
	if !strings.Contains(out, "Output index") {
		t.Errorf("missing Output index frame label\nout:\n%s", out)
	}
	if !strings.Contains(out, "1 / 4 parts indexed") {
		t.Errorf("missing live outputIdxMetrics parts counter\nout:\n%s", out)
	}
	// stale 99 GB must not appear
	if strings.Contains(out, "99.0 GB") {
		t.Errorf("stale phase-0 odMetrics leaked into phaseIndex frame\nout:\n%s", out)
	}
}

// regression for "[10]+ shifts the bar one column right". old fixed-4
// idx-marker width clipped rows 10-16, throwing alignment vs rows 1-9.
// every row must have same visible width up to bar's start column
func TestWorkerRowsAlignedAcrossDoubleDigitIndices(t *testing.T) {
	// >=10 to manifest. render direct via renderWorkerRow to skip
	// renderODFrame's termHeight dep that floors us on CI short terms
	const total = 16
	workers := make([]ulpengine.WorkerStatus, total)
	for i := range workers {
		name := fmt.Sprintf("sfu_run_part%d.txt.zst", i+1)
		workers[i].ArchivePath.Store(&name)
		workers[i].PartIdx.Store(int32(i + 1))
		workers[i].PartsTotal.Store(total)
		// keep bytesDone < 1 GB so humanBytes stays at MB (7-8 chars),
		// else dynamic rightW shifts bar column for variable totals
		workers[i].BytesDone.Store(int64(i+1) * 50 * 1024 * 1024)
		workers[i].BytesTotal.Store(2 * 1024 * 1024 * 1024)
	}

	idxW := workerIdxMarkerWidth(total)
	rowWidth := 180
	var rows []string
	for i := range workers {
		rows = append(rows, renderWorkerRow(i, &workers[i], rowWidth, idxW))
	}

	var firstBarCol = -1
	for i, ln := range rows {
		plain := stripANSI(ln)
		col := strings.IndexAny(plain, "▆░")
		if col < 0 {
			t.Fatalf("row %d has no bar char: %q", i, plain)
		}
		if i == 0 {
			firstBarCol = col
			continue
		}
		if col != firstBarCol {
			t.Errorf("row %d bar at col %d; want %d (rows 10+ shifted, alignment broken)\nrow 0: %q\nrow %d: %q",
				i, col, firstBarCol, stripANSI(rows[0]), i, plain)
		}
	}
}

// width formula direct, companion to per-row alignment test
func TestWorkerIdxMarkerWidth(t *testing.T) {
	cases := []struct {
		count int
		want  int
	}{
		{0, 4},   // floor
		{1, 4},   // "[1] "
		{9, 4},   // "[9] "
		{10, 5},  // "[10] "
		{16, 5},  // "[16] "
		{99, 5},  // "[99] "
		{100, 6}, // "[100] "
	}
	for _, c := range cases {
		if got := workerIdxMarkerWidth(c.count); got != c.want {
			t.Errorf("workerIdxMarkerWidth(%d) = %d, want %d", c.count, got, c.want)
		}
	}
}

// strips CSI escapes for visible-width counting
func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == 0x1b && s[i+1] == '[' {
			j := i + 2
			for j < len(s) {
				c := s[j]
				if c >= 0x40 && c <= 0x7e {
					j++
					break
				}
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
