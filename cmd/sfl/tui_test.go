package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

func TestWorkerPathLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"top level path unchanged", "/data/leak/outer.rar", "/data/leak/outer.rar"},
		{"nested collapses to outer and inner", "/data/leak/outer.rar!sub/dir/inner.7z", "outer.rar ▸ inner.7z"},
		{"nested inner without slash", "/data/outer.zip!inner.rar", "outer.zip ▸ inner.rar"},
		{"deep nesting keeps outer and innermost", "/data/a.rar!mid/b.7z!deep/c.zip", "a.rar ▸ c.zip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workerPathLabel(tc.in); got != tc.want {
				t.Fatalf("workerPathLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderFinalSummaryShowsSnowFastLogStats(t *testing.T) {
	lines := renderFinalSummary("out/sfl.txt", sflog.ExtractStats{
		FilesScanned:    4,
		ArchivesScanned: 2,
		Logs:            3,
		Credentials:     10,
		Emitted:         8,
		Duplicates:      2,
	})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"SnowFastLog",
		"COMPLETE",
		"Logs",
		"10 parsed",
		"Unique",
		"2 duplicates",
		"Sources",
		"2 archives",
		"out/sfl.txt",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderFinalSummaryUpdateNoticeFooter(t *testing.T) {
	lines := renderFinalSummaryWithNotice("out/sfl.txt", sflog.ExtractStats{
		Emitted: 1,
	}, &selfupdate.Notice{Latest: "9.9.9", Command: "sfl update"})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Update available: v9.9.9", "sfl update", "snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing update notice %q:\n%s", want, joined)
		}
	}
}

func TestRenderIngestSummaryShowsNewVsAlreadyAndLibrarySize(t *testing.T) {
	lines := renderIngestSummary("/data/Library", 1234, 5, 3, sflog.ExtractStats{
		Logs:        2,
		Credentials: 8,
		Emitted:     8,
	})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"INGESTED",
		"new",
		"already in library",
		"lines in library",
		"1,234",
		"/data/Library",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ingest summary missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderNoIngestSummaryReportsLibraryUnchanged(t *testing.T) {
	lines := renderNoIngestSummary("/data/Library", sflog.ExtractStats{
		Logs:             1,
		ArchivesScanned:  1,
		SkippedArchives:  1,
		PasswordNotFound: 1,
		Issues:           []sflog.Issue{{Path: "/data/locked.zip", Kind: sflog.IssuePasswordNotFound}},
	})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"COMPLETE",
		"library unchanged",
		"Library: ",
		"/data/Library",
		"password not found",
		"locked.zip",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("no-ingest summary missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderFinalSummaryReportsOpenErrors(t *testing.T) {
	lines := renderFinalSummary("out/sfl.txt", sflog.ExtractStats{
		Emitted:    3,
		OpenErrors: 2,
		Issues: []sflog.Issue{
			{Path: "/data/victimA-Passwords.txt", Kind: sflog.IssueOpenError},
			{Path: "/data/victimB-Passwords.txt", Kind: sflog.IssueOpenError},
		},
	})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"open errors", "victimA-Passwords.txt", "victimB-Passwords.txt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderProgressScanningStateIsCenteredWithSpinner(t *testing.T) {
	prog := sflog.NewProgress() // discovery phase, unknown total
	lines := renderProgress(0, prog, 0, 0, 80)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"[sfl]", "SCANNING", "discovering sources"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("scanning frame missing %q:\n%s", want, joined)
		}
	}
	// title, box, and footer all inset/right-aligned past leftPad (blank
	// separator rows excepted).
	pad := strings.Repeat(" ", sflLeftPad)
	for i, ln := range lines {
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, pad) {
			t.Fatalf("line %d is not inset by leftPad: %q", i, ln)
		}
	}
}

func TestRenderProgressShowsFooter(t *testing.T) {
	prog := sflog.NewProgress()
	joined := strings.Join(renderProgress(0, prog, 0, 0, 80), "\n")
	for _, want := range []string{"sfl is open-source", "snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frame missing footer %q:\n%s", want, joined)
		}
	}
}

func TestRenderSflWorkerPanelShowsConcurrentStages(t *testing.T) {
	active := []sflog.ActiveWorker{
		{Index: 0, Path: "/data/@beetraffic 3300 MIX.zip", Stage: sflog.StageTestingPassword},
		{Index: 1, Path: "/data/Flores Private Cloud 32.rar", Stage: sflog.StageExtracting},
		{Index: 3, Path: "/data/victim/Passwords.txt", Stage: sflog.StageParsing},
	}
	joined := strings.Join(renderSflWorkerPanel(active, 4, 72, 0), "\n")
	for _, want := range []string{
		"3 of 4 workers active",
		"testing password",
		"extracting",
		"parsing",
		"[1]", "[2]", "[4]",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("worker panel missing %q:\n%s", want, joined)
		}
	}
}

// TestRenderSflWorkerPanelHidesHeaderWhenOne proves a single active row drops
// the "N of M workers active" header (so a correctly one-stream archive doesn't
// look like wasted cores) but still shows the worker row, and that 2+ active
// restores the honest count.
func TestRenderSflWorkerPanelHidesHeaderWhenOne(t *testing.T) {
	one := []sflog.ActiveWorker{{Index: 0, Path: "/data/big.rar", Stage: sflog.StageExtracting}}
	joined := strings.Join(renderSflWorkerPanel(one, 16, 72, 0), "\n")
	if strings.Contains(joined, "workers active") {
		t.Fatalf("single active worker must not show the count header:\n%s", joined)
	}
	if !strings.Contains(joined, "extracting") || !strings.Contains(joined, "[1]") {
		t.Fatalf("single worker row missing:\n%s", joined)
	}
	two := append(one, sflog.ActiveWorker{Index: 1, Path: "/data/b.zip", Stage: sflog.StageParsing})
	if j := strings.Join(renderSflWorkerPanel(two, 16, 72, 0), "\n"); !strings.Contains(j, "2 of 16 workers active") {
		t.Fatalf("two active workers must show the count header:\n%s", j)
	}
}

// TestFormatETA covers the hide/show rules: hidden during discovery (no total ->
// non-positive remaining), near-complete, a stalled/unmeasured rate, and an
// absurd horizon; shown with a steady rate.
func TestFormatETA(t *testing.T) {
	cases := []struct {
		name      string
		remaining int64
		rate      float64
		wantETA   bool
	}{
		{"near-complete", 0, 10 << 20, false},
		{"discovery-negative", -5, 10 << 20, false},
		{"stalled-rate", 5 << 20, 0.5, false},
		{"absurd-horizon", int64(99*3600) * 4, 2, false},
		{"steady", 100 << 20, 10 << 20, true}, // ~10s
	}
	for _, c := range cases {
		got := formatETA(c.remaining, c.rate)
		if c.wantETA && !strings.Contains(got, "ETA ") {
			t.Errorf("%s: formatETA(%d,%v)=%q, want an ETA", c.name, c.remaining, c.rate, got)
		}
		if !c.wantETA && got != "" {
			t.Errorf("%s: formatETA(%d,%v)=%q, want empty", c.name, c.remaining, c.rate, got)
		}
	}
}

func TestRenderSflWorkerRowAnimatesSpinner(t *testing.T) {
	w := sflog.ActiveWorker{Index: 0, Path: "/data/a.zip", Stage: sflog.StageExtracting}
	// Successive ticks must change the braille spinner glyph so the row reads as
	// live motion rather than a static label.
	seen := map[string]bool{}
	for tick := 0; tick < len(workerSpinnerFrames); tick++ {
		row := renderSflWorkerRow(w, 60, 4, tick)
		found := ""
		for _, f := range workerSpinnerFrames {
			if strings.Contains(row, f) {
				found = f
				break
			}
		}
		if found == "" {
			t.Fatalf("tick %d: row has no spinner glyph: %q", tick, row)
		}
		seen[found] = true
	}
	if len(seen) < 2 {
		t.Fatalf("spinner did not animate across ticks; saw frames %v", seen)
	}
}

func TestWorkerSpinnerCascadesByWorkerIndex(t *testing.T) {
	// At a fixed tick, adjacent worker rows should show different frames so the
	// panel ripples instead of blinking in unison.
	if a, b := workerSpinnerFrame(5, 0), workerSpinnerFrame(5, 1); a == b {
		t.Fatalf("expected phase-shifted frames for adjacent workers, both = %q", a)
	}
}

func TestRenderSflWorkerRowTruncatesLongPath(t *testing.T) {
	w := sflog.ActiveWorker{Index: 2, Path: "/very/long/path/" + strings.Repeat("x", 200) + "/Passwords.txt", Stage: sflog.StageExtracting}
	row := renderSflWorkerRow(w, 60, 4, 0)
	if !strings.Contains(row, "[3]") {
		t.Fatalf("row missing 1-based marker: %q", row)
	}
	if !strings.Contains(row, "extracting") {
		t.Fatalf("row missing stage: %q", row)
	}
	if len([]rune(row)) > 60+40 { // styled escapes add width; sanity ceiling
		t.Fatalf("row not truncated to inner width: %d runes", len([]rune(row)))
	}
}

func TestSflWorkerRowCap(t *testing.T) {
	cases := []struct {
		name          string
		height, total int
		want          int
	}{
		{"no workers", 50, 0, 0},
		{"very short term keeps floor", 16, 16, sflMaxWorkerRows},
		{"floor capped by total", 16, 4, 4},
		{"mid term below total", 24, 16, 12},
		{"tall term expands toward total", 60, 16, 16},
		{"tall term capped at total", 100, 12, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sflWorkerRowCap(tc.height, tc.total); got != tc.want {
				t.Fatalf("sflWorkerRowCap(%d, %d) = %d, want %d", tc.height, tc.total, got, tc.want)
			}
		})
	}
}

func TestRenderFinalSummaryReportsSkippedCount(t *testing.T) {
	lines := renderFinalSummary("out/sfl.txt", sflog.ExtractStats{
		ArchivesScanned:  3,
		Emitted:          4,
		SkippedArchives:  2,
		SkippedFiles:     1,
		PasswordNotFound: 2,
		Issues: []sflog.Issue{
			{Path: "/data/locked.zip", Kind: sflog.IssuePasswordNotFound},
		},
	})
	joined := strings.Join(lines, "\n")
	// The recap count row must surface the total skipped sources alongside the
	// per-kind fail lines, so failures are never hidden from the end summary.
	for _, want := range []string{"3 skipped", "2 passwords not found", "locked.zip"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
}

func TestMutedIssueBlockPluralizeByCount(t *testing.T) {
	one := strings.Join(mutedIssueBlock(sflog.ExtractStats{
		ParseErrors: 1, OpenErrors: 1, PasswordNotFound: 1, MissingVolumes: 1, NoULP: 1,
	}, 80), "\n")
	for _, want := range []string{"1 parse error", "1 open error", "1 password not found", "1 incomplete set", "1 source with no ULP"} {
		if !strings.Contains(one, want) {
			t.Fatalf("singular issue label missing %q:\n%s", want, one)
		}
	}
	if strings.Contains(one, "errors") || strings.Contains(one, "passwords") || strings.Contains(one, "sets") || strings.Contains(one, "sources") {
		t.Fatalf("singular counts should not pluralize:\n%s", one)
	}

	many := strings.Join(mutedIssueBlock(sflog.ExtractStats{
		ParseErrors: 3, OpenErrors: 2, PasswordNotFound: 4, MissingVolumes: 6, NoULP: 5,
	}, 80), "\n")
	for _, want := range []string{"3 parse errors", "2 open errors", "4 passwords not found", "6 incomplete sets", "5 sources with no ULP"} {
		if !strings.Contains(many, want) {
			t.Fatalf("plural issue label missing %q:\n%s", want, many)
		}
	}
}

// TestSummaryIssuesAppearAboveBoxMuted proves failures render in the grey block
// ABOVE the stats box (not inside it), carry the muted style rather than the
// yellow warn style, and show full inner/outer provenance.
func TestSummaryIssuesAppearAboveBoxMuted(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	lines := renderIngestSummary("/data/Library", 1000, 1, 1, sflog.ExtractStats{
		Logs: 1, Credentials: 1, Emitted: 1,
		PasswordNotFound: 2,
		Issues: []sflog.Issue{
			{Path: "/logs/a.rar!sub/locked.zip", Kind: sflog.IssuePasswordNotFound},
			{Path: "/logs/b.rar!sub/sealed.7z", Kind: sflog.IssuePasswordNotFound},
		},
	})

	headerIdx, borderIdx := -1, -1
	for i, ln := range lines {
		if headerIdx < 0 && strings.Contains(ln, "passwords not found") {
			headerIdx = i
		}
		if borderIdx < 0 && strings.Contains(ln, "╭") {
			borderIdx = i
		}
	}
	joined := strings.Join(lines, "\n")
	if headerIdx < 0 || borderIdx < 0 {
		t.Fatalf("header (%d) or box border (%d) not found:\n%s", headerIdx, borderIdx, joined)
	}
	if headerIdx >= borderIdx {
		t.Fatalf("issue header at %d is not above the box border at %d:\n%s", headerIdx, borderIdx, joined)
	}
	for i := borderIdx; i < len(lines); i++ {
		if strings.Contains(lines[i], "passwords not found") {
			t.Fatalf("issue header leaked into/after the box at line %d:\n%s", i, joined)
		}
	}
	if !strings.Contains(joined, "locked.zip  —  in a.rar") {
		t.Fatalf("missing inner/outer provenance form:\n%s", joined)
	}
	// #8BA7B1 (muted grey), never #F2C14E (warn yellow), in a completion frame.
	if !strings.Contains(lines[headerIdx], "139;167;177") {
		t.Fatalf("issue header is not muted-styled: %q", lines[headerIdx])
	}
	if strings.Contains(joined, "242;193;78") {
		t.Fatalf("warn (yellow) styling must not appear in a completion summary:\n%s", joined)
	}
}

// TestSummarySurfacesMissingVolume guards the newly surfaced incomplete-set
// kind, which the prior in-box block never reported.
func TestSummarySurfacesMissingVolume(t *testing.T) {
	lines := renderFinalSummary("out/sfl.txt", sflog.ExtractStats{
		Emitted:        3,
		MissingVolumes: 1,
		Issues: []sflog.Issue{
			{Path: "/data/set.part2.rar", Kind: sflog.IssueMissingVolume},
		},
	})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"1 incomplete set", "set.part2.rar"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
}

func TestTrimToDisplayWidthClampsStyledMultibyte(t *testing.T) {
	// ANSI-styled, multibyte content far wider than the cap (mirrors a worker
	// row with a long unicode path and the "▸" nested separator).
	styled := sflOkStyle.Render("extracting") + "  " +
		sflMutedStyle.Render("/data/"+strings.Repeat("é", 120)+"/Passwords.txt ▸ inner.7z")
	for _, max := range []int{1, 10, 20, 34, 72} {
		got := trimToDisplayWidth(styled, max)
		if w := tuiVisibleWidth(got); w > max {
			t.Fatalf("trimToDisplayWidth(_, %d) visible width = %d (> %d): %q", max, w, max, got)
		}
	}
	// A line already within budget is returned untouched.
	short := sflOkStyle.Render("ok")
	if got := trimToDisplayWidth(short, 40); got != short {
		t.Fatalf("short line altered: %q -> %q", short, got)
	}
}

// TestSflFrameRowsClampToWidth proves the draw() width clamp keeps every
// composed worker-panel row within the terminal width on terminals narrower
// than the box floor, so rows can't soft-wrap and ghost.
func TestSflFrameRowsClampToWidth(t *testing.T) {
	active := []sflog.ActiveWorker{
		{Index: 0, Path: "/data/" + strings.Repeat("x", 200) + ".zip", Stage: sflog.StageExtracting},
		{Index: 1, Path: "/data/outer.rar!sub/" + strings.Repeat("y", 80) + "/inner.7z", Stage: sflog.StageTestingPassword},
	}
	rows := renderSflWorkerPanel(active, 4, 72, 0)
	for _, w := range []int{8, 12, 24, 34} {
		for _, ln := range rows {
			got := trimToDisplayWidth(ln, w)
			if vw := tuiVisibleWidth(got); vw > w {
				t.Fatalf("row clamped to %d but visible width %d: %q", w, vw, got)
			}
		}
	}
}

func TestRenderInterruptShowsCleanupNotice(t *testing.T) {
	lines := renderInterrupt(0, "/", 80)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"INTERRUPTED", "force-exit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("interrupt frame missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderFinalSummaryReportsIssues(t *testing.T) {
	lines := renderFinalSummary("out/sfl.txt", sflog.ExtractStats{
		ArchivesScanned:  3,
		Emitted:          5,
		SkippedArchives:  2,
		PasswordNotFound: 2,
		NoULP:            1,
		Issues: []sflog.Issue{
			{Path: "/data/locked.zip", Kind: sflog.IssuePasswordNotFound},
			{Path: "/data/sealed.7z", Kind: sflog.IssuePasswordNotFound},
			{Path: "/data/victim/Passwords.txt", Kind: sflog.IssueNoULP},
		},
	})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"2 passwords not found",
		"locked.zip",
		"sealed.7z",
		"source with no ULP",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
}
