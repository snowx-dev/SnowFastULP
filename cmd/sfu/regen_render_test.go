package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// worker row count scales with remaining vertical room. ceiling =
// maxWorkerRowsRendered AND totalWorkers; the height budget subtracts the
// frame's non-worker rows, so a short terminal yields fewer rows (never a
// dropped footer) instead of the old optimistic floor.
func TestWorkerRowCapAdapts(t *testing.T) {
	tests := []struct {
		name                    string
		termHeight, totalWorker int
		nonWorkerRows           int
		want                    int
	}{
		{"generous term caps at 8", 60, 16, 0, maxWorkerRowsRendered},
		{"capped by totalWorkers", 60, 3, 0, 3},
		{"height budget between ceiling and workers", 24, 16, 6, 8},
		{"height budget below ceiling", 24, 16, 18, 5},
		{"exhausted budget yields zero, keeps footer", 24, 16, 25, 0},
		{"negative budget clamps to zero", 10, 16, 20, 0},
		{"zero workers", 40, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workerRowCap(tt.termHeight, tt.totalWorker, tt.nonWorkerRows); got != tt.want {
				t.Errorf("workerRowCap(%d, %d, %d) = %d, want %d",
					tt.termHeight, tt.totalWorker, tt.nonWorkerRows, got, tt.want)
			}
		})
	}
}

// the cap is min(totalWorkers, maxWorkerRowsRendered, max(0, h-1-nonWorkerRows)):
// it must never exceed 8, and every positive result must leave at least one
// row of slack under the termHeight-1 Compose clamp.
func TestWorkerRowCapNeverOverflowsTerminal(t *testing.T) {
	for h := 5; h <= 80; h++ {
		for _, nw := range []int{0, 6, 12, 18, 24} {
			got := workerRowCap(h, 32, nw)
			if got > maxWorkerRowsRendered {
				t.Errorf("h=%d nw=%d: cap=%d exceeds ceiling %d", h, nw, got, maxWorkerRowsRendered)
			}
			if got > 0 && h-1-(nw+got) < 0 {
				t.Errorf("h=%d nw=%d: cap=%d overflows clamp budget", h, nw, got)
			}
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

	out := strings.Join(renderODFrame(m, 0, 100, 0, ""), "\n")
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

	out := strings.Join(renderODFrame(m, 0, 100, 0, ""), "\n")
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

	lines := renderODFrame(m, 0, 100, 0, "")
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

// overflow = one bullet per line, indented under label when indent fits.
// repros the "Removed ... 102M already i…" truncation bug
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
	for i := 1; i < len(rows); i++ {
		plain := stripANSI(rows[i])
		if strings.Contains(plain, "Removed") {
			t.Errorf("continuation row %d should NOT repeat the label, got %q", i, plain)
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

// mixed bullet widths must keep full counts visible; indent drops only when
// indent+bullet would overflow the budget (Merge-style).
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
	joined := stripANSI(strings.Join(rows, "\n"))
	for _, want := range []string{"9", "999,999,999,999", "12,345,678", "rejected", "duplicates", "already in library"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	for i, row := range rows {
		if w := tuiVisibleWidth(row); w > 40 {
			t.Errorf("row %d width %d exceeds budget 40: %q", i, w, stripANSI(row))
		}
	}
}

func TestRenderRemovedRowsSurviveGradientBoxWithShares(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// Budget must fit the longest share-annotated bullet alone ("already in library").
	const inner = 40
	bullets := []string{
		warnStyle.Render("20") + mutedStyle.Render(tuistat.ShareParen(20, 200)) + " " + mutedStyle.Render("rejected"),
		countStyle.Render("5") + mutedStyle.Render(tuistat.ShareParen(5, 100)) + " " + mutedStyle.Render("duplicates"),
		countStyle.Render("15") + mutedStyle.Render(tuistat.ShareParen(15, 100)) + " " + mutedStyle.Render("already in library"),
	}
	rows := renderRemovedRows(bullets, inner)
	for i, row := range rows {
		if w := tuiVisibleWidth(row); w > inner {
			t.Fatalf("pre-box row %d width %d > %d: %q", i, w, inner, stripANSI(row))
		}
	}
	// gradientBox inner = outerWidth-6; use outer 46 → inner 40.
	joined := gradientBox(rows, 46, doneStart, doneEnd)
	for _, want := range []string{"rejected", "duplicates", "already in library", "(10.0%)", "(5.0%)", "(15.0%)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("boxed Removed missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("boxed Removed must not be ellipsized:\n%s", joined)
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

	lines := renderODFrame(m, 0, 120, 0, "")
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

// bytes hit 100% at stream EOF but the sidecar commit still runs: the pct
// stays an honest "100%" (bar full, pct budget fixed) while the BYTES
// column — frozen totals that never change during the commit window — is
// swapped for the "committing…" state label. a pre-commit row at 100%
// bytes keeps its totals and shows no label
func TestWorkerRowCommittingReplacesFrozen100Pct(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.ArchivesNeedRegen.Store(1)
	m.RegenBytesTotal.Store(1 << 30)
	m.RegenBytesRead.Store(1 << 30)
	m.Workers = make([]ulpengine.WorkerStatus, 1)
	name := "sfu_test_part1.txt.zst"
	ws := &m.Workers[0]
	ws.ArchivePath.Store(&name)
	ws.PartIdx.Store(1)
	ws.PartsTotal.Store(1)
	ws.BytesDone.Store(1 << 29)
	ws.BytesTotal.Store(1 << 29)

	findRow := func() string {
		for _, ln := range renderODFrame(m, 0, 100, 0, "") {
			if strings.Contains(ln, "[1]") {
				return ln
			}
		}
		return ""
	}

	// pre-commit: totals + percent, no state label
	row := stripANSI(findRow())
	if !strings.Contains(row, "100%") || !strings.Contains(row, " / ") {
		t.Errorf("row at 100%% bytes before commit should read 100%% with byte totals:\n%s", row)
	}
	if strings.Contains(row, "committing…") {
		t.Errorf("state label shown before the commit window:\n%s", row)
	}

	// commit window: pct still 100%, totals replaced by the label
	ws.Committing.Store(true)
	row = stripANSI(findRow())
	if !strings.Contains(row, "100%") {
		t.Errorf("committing row must keep the numeric 100%% pct:\n%s", row)
	}
	if !strings.Contains(row, "committing…") {
		t.Errorf("committing row missing 'committing…' state in the bytes column:\n%s", row)
	}
	if strings.Contains(row, " / ") {
		t.Errorf("committing row still shows frozen byte totals:\n%s", row)
	}
}

// all bytes read but parts still commit sidecars: the throughput row must
// hold a wordless muted ellipsis instead of a stale "0 B/s". mid-stream and
// post-commit keep the rate render, and the row never disappears (frame
// height stable)
func TestThroughputHoldsEllipsisWhileSidecarsCommit(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.RegenBytesTotal.Store(1 << 30)
	m.RegenBytesRead.Store(1 << 30)
	m.PartsRegenTotal.Store(8)
	m.PartsRegenDone.Store(2)

	out := stripANSI(strings.Join(renderODFrame(m, 0, 100, 0, ""), "\n"))
	if !strings.Contains(out, "Throughput") || !strings.Contains(out, "…") {
		t.Errorf("bytes done, parts pending: want the muted ellipsis placeholder\nout:\n%s", out)
	}
	if strings.Contains(out, "B/s") {
		t.Errorf("stale rate shown while sidecars commit\nout:\n%s", out)
	}

	// mid-stream: honest rate, no placeholder
	m.RegenBytesRead.Store(1 << 29)
	out = stripANSI(strings.Join(renderODFrame(m, 0, 100, 0, ""), "\n"))
	if !strings.Contains(out, "B/s") || strings.Contains(out, "…") {
		t.Errorf("mid-stream should show a live rate:\n%s", out)
	}

	// last commit lands: back to the plain rate render, no placeholder
	m.RegenBytesRead.Store(1 << 30)
	m.PartsRegenDone.Store(8)
	out = stripANSI(strings.Join(renderODFrame(m, 0, 100, 0, ""), "\n"))
	if strings.Contains(out, "…") {
		t.Errorf("ellipsis kept after the last commit\nout:\n%s", out)
	}

	// legacy archive-grained regen: no parts denominator, archives counter
	// instead — bytes done but archives unfinished must also read as the
	// placeholder, not "0 B/s"
	legacy := &ulpengine.ODMetrics{}
	legacy.Phase.Store(int32(ulpengine.ODPhaseRegen))
	legacy.RegenBytesTotal.Store(1 << 30)
	legacy.RegenBytesRead.Store(1 << 30)
	legacy.ArchivesNeedRegen.Store(4)
	legacy.ArchivesRegenedDone.Store(1)
	out = stripANSI(strings.Join(renderODFrame(legacy, 0, 100, 0, ""), "\n"))
	if !strings.Contains(out, "Throughput") || !strings.Contains(out, "…") {
		t.Errorf("legacy archive-grained regen, bytes done but archives pending: want the muted ellipsis placeholder\nout:\n%s", out)
	}
	if strings.Contains(out, "B/s") {
		t.Errorf("stale rate shown while legacy regen finalizes\nout:\n%s", out)
	}
	legacy.ArchivesRegenedDone.Store(4)
	out = stripANSI(strings.Join(renderODFrame(legacy, 0, 100, 0, ""), "\n"))
	if strings.Contains(out, "…") {
		t.Errorf("ellipsis kept after the last archive\nout:\n%s", out)
	}
}

// the system RAM/CPU row must ride INSIDE the OD box (a bordered row), and
// an empty systemRow must render nothing (phase 1/2 boxes carry their own)
func TestRenderODFrameSystemRowInsideBox(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.RegenBytesTotal.Store(1 << 30)
	m.RegenBytesRead.Store(1 << 29)

	lines := renderODFrame(m, 0, 100, 0, renderSystemRow(123.5, 42))
	sysIdx, boxTop, boxBottom := -1, -1, -1
	for i, ln := range lines {
		plain := stripANSI(ln)
		switch {
		case strings.Contains(plain, "System"):
			sysIdx = i
		case strings.Contains(plain, "╭"):
			boxTop = i
		case strings.Contains(plain, "╰"):
			boxBottom = i
		}
	}
	if sysIdx < 0 {
		t.Fatalf("frame missing System row\nlines:\n%s", strings.Join(lines, "\n"))
	}
	if boxTop < 0 || boxBottom < 0 || sysIdx <= boxTop || sysIdx >= boxBottom {
		t.Fatalf("System row not between box borders (top=%d, sys=%d, bottom=%d)\nlines:\n%s",
			boxTop, sysIdx, boxBottom, strings.Join(lines, "\n"))
	}
	if !strings.Contains(stripANSI(lines[sysIdx]), "│") {
		t.Errorf("System row rendered without box borders:\n%s", lines[sysIdx])
	}

	// empty systemRow = phase 1/2 behavior: no System row in the OD box
	lines = renderODFrame(m, 0, 100, 0, "")
	if strings.Contains(stripANSI(strings.Join(lines, "\n")), "System") {
		t.Errorf("empty systemRow leaked a System row into the OD box\nlines:\n%s",
			strings.Join(lines, "\n"))
	}
}
