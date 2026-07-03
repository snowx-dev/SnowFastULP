package main

import (
	"strings"
	"testing"
	"time"

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
	// Emitted 10 == Added 5 + already 3 + dropped 2, the invariant the recap shows.
	lines := renderIngestSummary("/data/Library", 1234, 5, 3, 2, sflog.ExtractStats{
		Logs:        2,
		Credentials: 10,
		Emitted:     10,
	}, []string{"/data/Library/sfu_20260701_part1.txt.zst"})
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"INGESTED",
		"Added",
		"entries",
		"Removed",
		"rejected",
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
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("no-ingest summary missing %q:\n%s", want, joined)
		}
	}
	// Issues are streamed to the -err file, not the stdout summary.
	for _, absent := range []string{"password not found", "locked.zip"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("issue detail %q must not appear on stdout:\n%s", absent, joined)
		}
	}
}

func TestRenderFinalSummaryOmitsOpenErrorsFromStdout(t *testing.T) {
	lines := renderFinalSummary("out/sfl.txt", sflog.ExtractStats{
		Emitted:    3,
		OpenErrors: 2,
		Issues: []sflog.Issue{
			{Path: "/data/victimA-Passwords.txt", Kind: sflog.IssueOpenError},
			{Path: "/data/victimB-Passwords.txt", Kind: sflog.IssueOpenError},
		},
	})
	joined := strings.Join(lines, "\n")
	// Issues live in the -err file now; stdout stays clean.
	for _, absent := range []string{"open issues", "victimA-Passwords.txt", "victimB-Passwords.txt"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("issue detail %q must not appear on stdout:\n%s", absent, joined)
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

func TestRenderSflBarPairLabelsBothTracks(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii) // strip ANSI so substring checks are stable
	rows := renderSflBarPair("Extract", 0.5, "Secrets", 0.25, false, 72)
	if len(rows) != 2 {
		t.Fatalf("want 2 bar rows, got %d", len(rows))
	}
	if !strings.Contains(rows[0], "Extract") || !strings.Contains(rows[0], "50.0%") {
		t.Fatalf("extract bar row wrong: %q", rows[0])
	}
	if !strings.Contains(rows[1], "Secrets") || !strings.Contains(rows[1], "25.0%") {
		t.Fatalf("secrets bar row wrong: %q", rows[1])
	}
}

// TestRenderSflBarPairSecretsPending covers the indeterminate Secrets bar:
// while a streaming source is open the denominator is not final, so the bar
// drops the percentage for a muted "----" trough and never claims 100%.
func TestRenderSflBarPairSecretsPending(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	rows := renderSflBarPair("Extract", 0.029, "Secrets", 1.0, true, 72)
	if strings.Contains(rows[1], "100.0%") {
		t.Fatalf("pending secrets bar must not show a percentage: %q", rows[1])
	}
	if !strings.Contains(rows[1], "----") {
		t.Fatalf("pending secrets bar should show the ---- trough: %q", rows[1])
	}
	if !strings.Contains(rows[1], "Secrets") {
		t.Fatalf("pending secrets bar lost its label: %q", rows[1])
	}
	// Extract bar is unaffected by the Secrets pending state.
	if !strings.Contains(rows[0], "2.9%") {
		t.Fatalf("extract bar should still show its real fraction: %q", rows[0])
	}
}

func TestRenderSecretsLiveRow(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	row := renderSecretsLiveRow(12, 340, 512, false)
	for _, want := range []string{"Secrets", "12", "found", "340", "512", "files scanned"} {
		if !strings.Contains(row, want) {
			t.Fatalf("secrets row missing %q: %q", want, row)
		}
	}
	if strings.Contains(row, "+") {
		t.Fatalf("non-streaming row should not carry the + signal: %q", row)
	}
}

// TestRenderSecretsLiveRowStreaming covers the "Y+" denominator signal: while a
// non-pre-counted source (rar/7z-encrypted/nested) is open, the total is not
// final, so the row appends "+" after Y. With no total yet, no "+" is shown
// (there is no denominator to mark incomplete).
func TestRenderSecretsLiveRowStreaming(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	row := renderSecretsLiveRow(3, 9, 9, true)
	if !strings.Contains(row, "9+") {
		t.Fatalf("streaming row should show Y+ (9+): %q", row)
	}
	// Streaming but no candidates credited yet: no denominator, no "+".
	zero := renderSecretsLiveRow(0, 0, 0, true)
	if strings.Contains(zero, "+") {
		t.Fatalf("streaming row with total=0 should not show +: %q", zero)
	}
}

func TestRenderProgressSecretsFinalizePhase(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.EnableSecrets()
	prog.BeginSecretsFinalize()
	joined := strings.Join(renderProgress(0, prog, 0, 0, 0, 80), "\n")
	for _, want := range []string{"FINALIZING SECRETS", "Extract", "Secrets", "writing to store"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("finalize frame missing %q:\n%s", want, joined)
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
		"extracting ulps",
		"parsing ulps",
		"[1]", "[2]", "[4]",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("worker panel missing %q:\n%s", want, joined)
		}
	}
}

// TestSflStageLabelExplicit covers the user-facing panel labels: the
// credential/secret actions name their target, and every label fits the fixed
// 16-cell stage column so paths stay aligned without a width change.
func TestSflStageLabelExplicit(t *testing.T) {
	cases := []struct {
		stage sflog.WorkerStage
		want  string
	}{
		{sflog.StageOpening, "opening"},
		{sflog.StageTestingPassword, "testing password"},
		{sflog.StageExtracting, "extracting ulps"},
		{sflog.StageParsing, "parsing ulps"},
		{sflog.StageScanning, "scanning secrets"},
		{sflog.WorkerStage(999), "working"}, // unknown stage falls back
	}
	for _, c := range cases {
		if got := sflStageLabel(c.stage); got != c.want {
			t.Fatalf("sflStageLabel(%v) = %q, want %q", c.stage, got, c.want)
		}
		if w := lipgloss.Width(sflStageLabel(c.stage)); w > sflStageColW {
			t.Fatalf("label %q is %d cells wide, exceeds sflStageColW=%d",
				c.want, w, sflStageColW)
		}
	}
	// StageScanning renders in a real row end-to-end (the -secrets tail label).
	row := renderSflWorkerRow(sflog.ActiveWorker{Index: 0, Path: "/data/a.env", Stage: sflog.StageScanning}, 60, 4, 0)
	if !strings.Contains(row, "scanning secrets") {
		t.Fatalf("scanning row missing explicit label:\n%s", row)
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
		rate     = 10 << 20 // 10MB/s shown on the Bytes row
	)
	joined := strings.Join(renderExtractStatsRows(files, archives, logs, logsTot, emitted, dupes, done, total, rate), "\n")
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
	if strings.Contains(joined, "ETA") {
		t.Fatalf("ETA row must be removed from stats rows:\n%s", joined)
	}
}

func TestRenderExtractStatsRowsLabels(t *testing.T) {
	rows := renderExtractStatsRows(10, 2, 3, 5, 7, 1, 1<<20, 10<<20, 5<<20)
	for _, label := range []string{"Logs", "Unique", "Sources", "Bytes"} {
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
	if len(rows) != 4 {
		t.Fatalf("want 4 stats rows (ETA removed), got %d", len(rows))
	}
}

func TestRenderIngestStatsRowsRegen(t *testing.T) {
	joined := strings.Join(renderIngestStatsRows(sflog.IngestView{
		EnginePhase:     ulpengine.PhasePhase0,
		PartsRegenDone:  3,
		PartsRegenTotal: 16,
		RegenBytesRead:  12 << 30,
		RegenBytesTotal: 80 << 30,
		RegenBPS:        128 << 20,
		ArchivesTotal:   8,
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

// TestSflSecretsWorkerRows locks the fixed-height sizing for the two-bar
// -secrets frame: as many worker rows as fit beneath the frame overhead, capped
// at sflMaxWorkerRows and the worker count, dropping to 0 on terminals too short
// for even one row.
func TestSflSecretsWorkerRows(t *testing.T) {
	cases := []struct {
		name          string
		height, total int
		want          int
	}{
		{"no workers", 50, 0, 0},
		{"too short for any row drops panel", 20, 16, 0},
		{"default 24-row term fits one", 24, 16, 1},
		{"31-row term hits the cap", 31, 16, sflMaxWorkerRows},
		{"tall term stays at cap", 60, 16, sflMaxWorkerRows},
		{"capped by worker count", 40, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sflSecretsWorkerRows(tc.height, tc.total); got != tc.want {
				t.Fatalf("sflSecretsWorkerRows(%d, %d) = %d, want %d", tc.height, tc.total, got, tc.want)
			}
		})
	}
}

// TestSflSecretsFrameFitsTerminal is the footer-flicker guard: the two-bar
// -secrets frame height is exactly sflSecretsFrameOverhead + worker rows, and
// the row count is chosen so that total never exceeds termHeight-1 (the draw()
// Compose clamp). If it did, the footer would be truncated on short terminals
// and redrawn as the busy-worker count changed — the flicker the fix removes.
// Because the row count depends only on terminal height and worker count (not
// the live active count), the footer keeps a constant screen row.
func TestSflSecretsFrameFitsTerminal(t *testing.T) {
	for h := 16; h <= 80; h++ {
		rows := sflSecretsWorkerRows(h, 16)
		if rows == 0 {
			continue // panel dropped; plain frame is well under any usable height
		}
		frameHeight := sflSecretsFrameOverhead + rows
		if frameHeight > h-1 {
			t.Fatalf("termHeight=%d: frame height %d exceeds clamp %d (footer would truncate)",
				h, frameHeight, h-1)
		}
	}
}

func TestRenderFinalSummaryReportsSkippedButOmitsIssueDetail(t *testing.T) {
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
	// The clean stats box still surfaces the total skipped sources...
	if !strings.Contains(joined, "3 skipped") {
		t.Fatalf("summary missing skipped count:\n%s", joined)
	}
	// ...but per-issue detail is streamed to the -err file, never stdout.
	for _, absent := range []string{"passwords not found", "locked.zip"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("issue detail %q must not appear on stdout:\n%s", absent, joined)
		}
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

// TestRenderIngestOutputFooterNothingNew proves a completed ingest that added
// nothing (all duplicates -> engine discarded the empty shard -> empty paths)
// states "(nothing new)" instead of silently dropping the Output row.
func TestRenderIngestOutputFooterNothingNew(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	joined := strings.Join(renderIngestOutputFooter(nil), "\n")
	if !strings.Contains(joined, "(nothing new)") {
		t.Fatalf("want (nothing new) footer for empty ingest, got:\n%s", joined)
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

// TestSflWorkerStageLabelBothRecent covers the "this archive is doing both right
// now" collapse: when a slot has pulled ULPs and scanned secrets within the
// recent window, the panel reads "ulp + secrets" regardless of the momentary
// stage; otherwise it falls back to the per-stage label.
func TestSflWorkerStageLabelBothRecent(t *testing.T) {
	now := time.Now()

	both := sflog.ActiveWorker{Stage: sflog.StageExtracting, LastULP: now, LastSec: now}
	if got := sflWorkerStageLabel(both); got != "ulp + secrets" {
		t.Fatalf("both recent: got %q, want \"ulp + secrets\"", got)
	}
	// Stage is Scanning but both activities are recent -> still collapses.
	bothScan := sflog.ActiveWorker{Stage: sflog.StageScanning, LastULP: now, LastSec: now}
	if got := sflWorkerStageLabel(bothScan); got != "ulp + secrets" {
		t.Fatalf("both recent (scanning stage): got %q, want \"ulp + secrets\"", got)
	}

	// Only secrets recent -> per-stage label.
	secOnly := sflog.ActiveWorker{Stage: sflog.StageScanning, LastSec: now}
	if got := sflWorkerStageLabel(secOnly); got != "scanning secrets" {
		t.Fatalf("sec only: got %q, want \"scanning secrets\"", got)
	}
	// Only ULP recent -> per-stage label.
	ulpOnly := sflog.ActiveWorker{Stage: sflog.StageExtracting, LastULP: now}
	if got := sflWorkerStageLabel(ulpOnly); got != "extracting ulps" {
		t.Fatalf("ulp only: got %q, want \"extracting ulps\"", got)
	}
	// Both stale -> per-stage label.
	stale := sflog.ActiveWorker{
		Stage:   sflog.StageExtracting,
		LastULP: now.Add(-10 * time.Second),
		LastSec: now.Add(-10 * time.Second),
	}
	if got := sflWorkerStageLabel(stale); got != "extracting ulps" {
		t.Fatalf("stale: got %q, want \"extracting ulps\"", got)
	}

	// The combined label fits the fixed stage column.
	if w := lipgloss.Width("ulp + secrets"); w > sflStageColW {
		t.Fatalf("\"ulp + secrets\" is %d cells, exceeds sflStageColW=%d", w, sflStageColW)
	}

	// End-to-end: a row built from a both-recent worker contains the combined label.
	row := renderSflWorkerRow(sflog.ActiveWorker{Index: 0, Path: "/data/a.rar", Stage: sflog.StageExtracting, LastULP: now, LastSec: now}, 60, 4, 0)
	if !strings.Contains(row, "ulp + secrets") {
		t.Fatalf("both-recent row missing combined label:\n%s", row)
	}
}
