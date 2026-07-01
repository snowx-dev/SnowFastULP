package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
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
	}, []string{"/data/Library/sfu_20260701_part1.txt.zst"})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"INGESTED",
		"entries",
		"Removed",
		"already in library",
		"lines in library",
		"1,234",
		"/data/Library",
		"sfu_20260701_part1.txt.zst",
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
	for _, want := range []string{"open issues", "victimA-Passwords.txt", "victimB-Passwords.txt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderProgressScanningStateIsCenteredWithSpinner(t *testing.T) {
	prog := sflog.NewProgress() // discovery phase, unknown total
	lines := renderProgress(0, prog, 0, 0, 0, 80)
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
	joined := strings.Join(renderProgress(0, prog, 0, 0, 0, 80), "\n")
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
		"3 workers active",
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
// the "N workers active" header (so a correctly one-stream archive doesn't
// look like wasted cores) but still shows the worker row, and that 2+ active
// restores the count header.
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
	if j := strings.Join(renderSflWorkerPanel(two, 16, 72, 0), "\n"); !strings.Contains(j, "2 workers active") {
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
		display := formatETADisplay(c.remaining, c.rate)
		if c.wantETA && display == "—" {
			t.Errorf("%s: formatETADisplay(%d,%v)=%q, want a duration", c.name, c.remaining, c.rate, display)
		}
		if !c.wantETA && display != "—" {
			t.Errorf("%s: formatETADisplay(%d,%v)=%q, want em dash", c.name, c.remaining, c.rate, display)
		}
	}
}

func TestSflRateEMAUpdate(t *testing.T) {
	const dt = 0.2 // monitor tick
	steady := float64(10 << 20) // 10 MB/s
	var ema float64
	for range 50 { // 10s of ticks
		ema = sflRateEMAUpdate(ema, steady, dt)
	}
	if ema < steady*0.9 {
		t.Fatalf("EMA after 10s steady input=%v, want ~%v", ema, steady)
	}
	// One burst tick at 10× should not move ETA rate more than ~25%.
	spike := sflRateEMAUpdate(ema, steady*10, dt)
	if spike > ema*1.25 {
		t.Fatalf("single spike EMA=%v, prev=%v; too reactive", spike, ema)
	}
	// Micro-stall (0 B/s) should decay slowly, not zero out.
	stall := sflRateEMAUpdate(ema, 0, dt)
	if stall < ema*0.95 {
		t.Fatalf("single zero tick EMA=%v, prev=%v; dropped too fast", stall, ema)
	}
}

func TestRenderExtractStatsRowsLargeNumbers(t *testing.T) {
	const (
		files    = 12_345_678
		archives = 678_901
		logs     = 1_234
		logsTot  = 5_678
		emitted  = 9_012_345
		dupes    = 3_456_789
		total    = 45 << 40 // ~45.0TB
		done     = total - (100 << 20)
		rate     = 10 << 20 // 10MB/s → ~10s ETA on 100MB remaining
	)
	joined := strings.Join(renderExtractStatsRows(files, archives, logs, logsTot, emitted, dupes, done, total, rate, rate), "\n")
	for _, want := range []string{
		"12,345,678", "678,901",
		"1,234", "5,678",
		"9,012,345", "3,456,789",
		"45.0TB", "10.0MB/s",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("large-number stats missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("stats rows must not truncate with ellipsis:\n%s", joined)
	}
	if !strings.Contains(joined, "10s") && !strings.Contains(joined, "9s") {
		t.Fatalf("expected ETA duration in stats rows:\n%s", joined)
	}
}

func TestRenderExtractStatsRowsETAHidden(t *testing.T) {
	joined := strings.Join(renderExtractStatsRows(1, 1, 1, 1, 1, 0, 100, 100, 0.5, 0), "\n")
	if !strings.Contains(joined, "—") {
		t.Fatalf("stalled rate should show em-dash ETA:\n%s", joined)
	}
}

func TestRenderExtractStatsRowsLabels(t *testing.T) {
	rows := renderExtractStatsRows(10, 2, 3, 5, 7, 1, 1<<20, 10<<20, 5<<20, 5<<20)
	for _, label := range []string{"Logs", "Unique", "Sources", "Bytes", "ETA"} {
		found := false
		for _, row := range rows {
			if strings.Contains(row, label) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing label %q in rows:\n%v", label, rows)
		}
	}
	if len(rows) != 5 {
		t.Fatalf("want 5 stats rows, got %d", len(rows))
	}
}

func TestRenderIngestStatsRowsRegen(t *testing.T) {
	joined := strings.Join(renderIngestStatsRows(sflog.IngestView{
		EnginePhase:       ulpengine.PhasePhase0,
		PartsRegenDone:    3,
		PartsRegenTotal:   16,
		RegenBytesRead:    12 << 30,
		RegenBytesTotal:   80 << 30,
		RegenBPS:          128 << 20,
		ArchivesTotal:     8,
	}), "\n")
	for _, want := range []string{"Library", "3", "16", "parts", "12.0GB", "80.0GB", "128.0MB/s"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("regen stats missing %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"Merge", "0 added", "already in library"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("regen stats should not contain %q:\n%s", bad, joined)
		}
	}
}

func TestRenderIngestStatsRowsShard(t *testing.T) {
	joined := strings.Join(renderIngestStatsRows(sflog.IngestView{
		EnginePhase: ulpengine.PhaseShard,
		ULPBytes:    189 << 20,
		BytesRead:   90 << 20,
		LinesRead:   1_200_000,
	}), "\n")
	for _, want := range []string{"ULP", "90.0MB", "189.0MB", "1,200,000"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("shard stats missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Merge") {
		t.Fatalf("shard stats should hide Merge row:\n%s", joined)
	}
}

func TestRenderIngestStatsRowsDedup(t *testing.T) {
	joined := strings.Join(renderIngestStatsRows(sflog.IngestView{
		EnginePhase:  ulpengine.PhaseDedup,
		ShowMerge:    true,
		Unique:       5,
		Skipped:      120,
		BucketsDone:  4,
		BucketsTotal: 64,
	}), "\n")
	for _, want := range []string{"Merge", "5", "added", "120", "already in library", "bucket", "4", "64"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("dedup stats missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderIngestRegenPanel(t *testing.T) {
	longName := "/data/lib/sfu_20260101_120000_part04.txt.zst"
	panel := renderIngestRegenPanel([]sflog.IngestWorker{
		{Archive: longName, PartIdx: 4, PartsTotal: 16, BytesDone: 1 << 30, BytesTotal: 2 << 30},
		{Archive: "/data/lib/sfu_20260101_120000_part05.txt.zst", PartIdx: 5, PartsTotal: 16, BytesDone: 512 << 20, BytesTotal: 1 << 30},
	}, 80, 0)
	if len(panel) < 3 {
		t.Fatalf("expected header + 2 worker rows, got %d:\n%v", len(panel), panel)
	}
	joined := strings.Join(panel, "\n")
	if !strings.Contains(joined, "2 workers active") {
		t.Fatalf("missing worker header:\n%s", joined)
	}
	if strings.Contains(joined, "sfu_") {
		t.Fatalf("archive name should be compacted:\n%s", joined)
	}
	if !strings.Contains(joined, "50%") {
		t.Fatalf("missing percent on worker row:\n%s", joined)
	}
}

func TestCompactIngestArchiveName(t *testing.T) {
	got := compactIngestArchiveName("/lib/sfu_20260101_120000_part04.txt.zst")
	if got != "20260101_120000_part04" {
		t.Fatalf("compact = %q", got)
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
		{"mid term below total", 24, 16, 9},
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
	for _, want := range []string{"1 parse issue", "1 open issue", "1 password not found", "1 incomplete set", "1 source with no ULP"} {
		if !strings.Contains(one, want) {
			t.Fatalf("singular issue label missing %q:\n%s", want, one)
		}
	}
	if strings.Contains(one, "passwords") || strings.Contains(one, "sets") || strings.Contains(one, "sources") {
		t.Fatalf("singular counts should not pluralize:\n%s", one)
	}

	many := strings.Join(mutedIssueBlock(sflog.ExtractStats{
		ParseErrors: 3, OpenErrors: 2, PasswordNotFound: 4, MissingVolumes: 6, NoULP: 5,
	}, 80), "\n")
	for _, want := range []string{"3 parse issues", "2 open issues", "4 passwords not found", "6 incomplete sets", "5 sources with no ULP"} {
		if !strings.Contains(many, want) {
			t.Fatalf("plural issue label missing %q:\n%s", want, many)
		}
	}
}

// TestSummaryIssuesAppearAboveBoxMuted proves failures render in the grey block
// ABOVE the stats box (not inside it), carry the muted style rather than the
// yellow warn style, and show full inner/outer provenance.
func TestMutedIssueBlockShowsDetail(t *testing.T) {
	block := strings.Join(mutedIssueBlock(sflog.ExtractStats{
		ParseErrors: 1,
		Issues: []sflog.Issue{
			{Path: "/data/bad.7z", Kind: sflog.IssueParseError, Err: sflog.ErrNotAnArchive},
		},
	}, 100), "\n")
	if !strings.Contains(block, "1 parse issue") {
		t.Fatalf("missing parse issue header:\n%s", block)
	}
	if !strings.Contains(block, "signature mismatch") {
		t.Fatalf("missing issue detail:\n%s", block)
	}
}

func TestRenderIngestOutputFooter(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	lines := renderIngestOutputFooter([]string{
		"/lib/sfu_20260701_part1.txt.zst",
		"/lib/sfu_20260701_part2.txt.zst",
	})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Output", "part1.txt.zst", "part2.txt.zst"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("footer missing %q:\n%s", want, joined)
		}
	}
}

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
	}, nil)

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
	// #A6818F (muted mauve), never #F2C14E (warn yellow), in a completion frame.
	if !strings.Contains(lines[headerIdx], "166;129;143") {
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
	lines := renderInterrupt(0, "/", 80, nil)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"INTERRUPTED", "force-exit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("interrupt frame missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderInterruptShowsCleanupLog(t *testing.T) {
	log := []string{"removed temp dir /tmp/sfl-spill-abc", "removed /out/sfl_partial.txt"}
	joined := strings.Join(renderInterrupt(0, "/", 80, log), "\n")
	for _, want := range log {
		if !strings.Contains(joined, want) {
			t.Fatalf("interrupt frame missing cleanup line %q:\n%s", want, joined)
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
