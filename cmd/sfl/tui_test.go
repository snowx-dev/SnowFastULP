package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/secrets"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// renderSflWorkerPanel is the pure variable-height worker-panel renderer kept as
// a test helper: a header count (only when 2+ busy) plus one row per busy slot.
// Production uses sflWorkerPanelBox (fixed-height, own box) which shares
// renderSflWorkerRow with this; this helper exercises the row renderer
// end-to-end with its header logic.
func renderSflWorkerPanel(active []sflog.ActiveWorker, total, inner, tick int) []string {
	if len(active) == 0 {
		return nil
	}
	idxMarkerW := lipgloss.Width(fmt.Sprintf("[%d]", total))
	out := make([]string, 0, len(active)+1)
	if len(active) >= 2 {
		out = append(out, sflLabelStyle.Render(fmt.Sprintf("%d workers active", len(active))))
	}
	for _, w := range active {
		out = append(out, renderSflWorkerRow(w, inner, idxMarkerW, tick))
	}
	return out
}

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
		"duplicates",
		"(80.0%)",
		"(20.0%)",
		"Sources",
		"2 archives",
		"out/sfl.txt",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
}

func TestRecapCountRowsUniqueSharesOmitWhenWhole(t *testing.T) {
	joined := strings.Join(recapCountRows(sflog.ExtractStats{
		Credentials: 8, Emitted: 8, Duplicates: 0,
	}), "\n")
	if strings.Contains(joined, "%") {
		t.Fatalf("all-unique recap must omit 100%% share:\n%s", joined)
	}
}

// Unique emitted+duplicates shares on one row mid-cut at default 80-col box
// (boxInner 66). Duplicates must ride a continuation like Secrets dupes.
func TestRecapCountRowsUniqueSharesSurviveDefaultBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	const width = 80
	inner := boxInner(width)
	if inner != 66 {
		t.Fatalf("boxInner(%d) = %d, want 66 (test assumption)", width, inner)
	}
	stats := sflog.ExtractStats{
		Logs: 1, Credentials: 2_000_000_000,
		Emitted: 1_000_000_000, Duplicates: 1_000_000_000,
		FilesScanned: 1, ArchivesScanned: 1,
	}
	joined := strings.Join(sflGradientBox(recapCountRows(stats), width, gradStart, gradEnd), "\n")
	for _, want := range []string{"Unique", "(50.0%)", "duplicates", "1,000,000,000"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("boxed Unique missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("boxed Unique must not be ellipsized by padOrTrim:\n%s", joined)
	}
}

func TestRenderIngestLibraryRowsSharesVsEmitted(t *testing.T) {
	joined := strings.Join(renderIngestLibraryRows(5, 3, 2, 10, 72, false), "\n")
	for _, want := range []string{"(50.0%)", "(30.0%)", "(20.0%)", "Added", "rejected", "already in library"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ingest library rows missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderSecretsBlockShares(t *testing.T) {
	joined := strings.Join(renderSecretsBlock(secrets.Stats{New: 2, Existing: 1, DupInRun: 1}, "", 80), "\n")
	for _, want := range []string{"(50.0%)", "(25.0%)", "new", "already stored", "dupes"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("secrets block missing %q:\n%s", want, joined)
		}
	}
}

// New+Existing shares on one Secrets row mid-cut at default width; Existing
// must ride a continuation (DupInRun already did).
func TestRenderSecretsBlockSharesSurviveDefaultBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	const width = 80
	inner := boxInner(width)
	if inner != 66 {
		t.Fatalf("boxInner(%d) = %d, want 66 (test assumption)", width, inner)
	}
	joined := strings.Join(renderSecretsBlock(secrets.Stats{
		New: 1_000_000_000, Existing: 1_000_000_000, DupInRun: 1_000_000_000,
	}, "", width), "\n")
	for _, want := range []string{"new", "already stored", "dupes", "(33.3%)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("boxed Secrets missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("boxed Secrets must not be ellipsized by padOrTrim:\n%s", joined)
	}
}

func TestRenderEnvLiveRow(t *testing.T) {
	row := renderEnvLiveRow(12)
	if !strings.Contains(row, "12") || !strings.Contains(row, "copied") {
		t.Fatalf("unexpected env live row: %q", row)
	}
}

func TestRecapCountRowsIncludesEnv(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvCopied: 3})
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "Env files") || !strings.Contains(joined, "3") || !strings.Contains(joined, "copied") {
		t.Fatalf("recap missing env row:\n%s", joined)
	}
	if strings.Contains(joined, "context") {
		t.Fatalf("recap should not mention context anymore:\n%s", joined)
	}
}

func TestRecapCountRowsIncludesTdata(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvDirsCopied: 2})
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "tdata") || !strings.Contains(joined, "2") || !strings.Contains(joined, "folder") {
		t.Fatalf("recap missing tdata row:\n%s", joined)
	}
	if strings.Contains(joined, "Env files") {
		t.Fatalf("EnvCopied==0 should omit Env files row:\n%s", joined)
	}
}

func TestRecapCountRowsIncludesOverCap(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvSkippedOverCap: 2})
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "Env skip") || !strings.Contains(joined, "over size cap") || !strings.Contains(joined, "2") {
		t.Fatalf("recap missing env over-cap row:\n%s", joined)
	}
	if strings.Contains(joined, "tdata skip") {
		t.Fatalf("flat env over-cap must not use tdata skip row:\n%s", joined)
	}
}

func TestRecapCountRowsIncludesTdataOverCap(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvDirsSkippedOverCap: 3})
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "tdata skip") || !strings.Contains(joined, "over size cap") || !strings.Contains(joined, "3") {
		t.Fatalf("recap missing tdata over-cap row:\n%s", joined)
	}
	if strings.Contains(joined, "Env skip") {
		t.Fatalf("tdata over-cap must not use Env skip row:\n%s", joined)
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
	}, []string{"/data/Library/sfu_20260701_part1.txt.zst"}, false)
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
	}, false)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"COMPLETE",
		"library unchanged",
		"Library",
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

// TestRenderSflBarPairSecretsPending covers the hidden Secrets bar: while a
// streaming source is open the denominator is not final, so the bar is replaced
// by a muted "scanning…" hint on the same row — no trough, no percentage — and
// the row slot stays so the fixed-height frame doesn't shift. The Extract bar
// keeps its real fraction.
func TestRenderSflBarPairSecretsPending(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	rows := renderSflBarPair("Extract", 0.029, "Secrets", 1.0, true, 72)
	if strings.Contains(rows[1], "100.0%") {
		t.Fatalf("pending secrets row must not show a percentage: %q", rows[1])
	}
	if !strings.Contains(rows[1], "scanning") {
		t.Fatalf("pending secrets row should show the scanning… hint: %q", rows[1])
	}
	if strings.Contains(rows[1], "----") {
		t.Fatalf("pending secrets row should not render the old trough: %q", rows[1])
	}
	if !strings.Contains(rows[1], "Secrets") {
		t.Fatalf("pending secrets row lost its label: %q", rows[1])
	}
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

// TestMatcherBadgeGating locks the go-regex warning: never when -secrets is off,
// silent on the libhs build, and a tasteful "build with -tags vectorscan + libhs"
// nudge on the slow pure-Go build — demoted or dropped on narrow terminals so it
// never crowds out the elapsed clock.
func TestMatcherBadgeGating(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	base := sflOkStyle.Render("[sfl] EXTRACTING")
	if got := matcherBadge(false, base, 110); got != "" {
		t.Fatalf("badge must be empty when -secrets is off, got %q", got)
	}
	switch secrets.MatcherBackend {
	case "libhs":
		if got := matcherBadge(true, base, 110); got != "" {
			t.Fatalf("libhs build must not warn, got %q", got)
		}
	case "go-regex":
		got := matcherBadge(true, base, 110)
		if !strings.Contains(got, "go-regex") || !strings.Contains(got, "libhs") {
			t.Fatalf("go-regex build should warn with the libhs nudge, got %q", got)
		}
		// Narrow terminal: drop entirely rather than crowd the clock/wrap.
		if narrow := matcherBadge(true, base, 50); narrow != "" {
			t.Fatalf("narrow terminal should drop the badge, got %q", narrow)
		}
	default:
		t.Fatalf("unknown MatcherBackend %q", secrets.MatcherBackend)
	}
}

// TestRenderProgressShowsRegexWarningOnSecretsRun threads the warning through a
// real frame: a -secrets run on the go-regex build surfaces it in the live
// header (finalizing phase here, since extraction needs unexported setters),
// while a libhs build stays silent and a non-secrets run never warns.
func TestRenderProgressShowsRegexWarningOnSecretsRun(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.EnableSecrets()
	prog.BeginSecretsFinalize()
	joined := strings.Join(renderProgress(0, prog, 0, 0, 0, 80), "\n")
	if secrets.MatcherBackend == "go-regex" {
		if !strings.Contains(joined, "go-regex") || !strings.Contains(joined, "use libhs") {
			t.Fatalf("go-regex secrets run should surface the libhs warning:\n%s", joined)
		}
	} else if strings.Contains(joined, "go-regex") {
		t.Fatalf("libhs build must not show the go-regex warning:\n%s", joined)
	}
	plain := strings.Join(renderProgress(0, sflog.NewProgress(), 0, 0, 0, 80), "\n")
	if strings.Contains(plain, "go-regex") {
		t.Fatalf("non-secrets run must not show the matcher warning:\n%s", plain)
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
	}, 72), "\n")
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
	}, 72), "\n")
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
	}, 72), "\n")
	for _, want := range []string{"Merge", "5", "added", "120", "already in library", "bucket", "4", "64"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("dedup stats missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderIngestMergeRowsSingleLineWhenWide(t *testing.T) {
	iv := sflog.IngestView{
		ShowMerge: true, Unique: 5, Skipped: 120, BucketsDone: 4, BucketsTotal: 64,
	}
	rows := renderIngestMergeRows(iv, 72)
	if len(rows) != 1 {
		t.Fatalf("wide Merge should be one line, got %d:\n%v", len(rows), rows)
	}
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"Merge", "added", "already in library", "bucket", "4", "64"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wide Merge missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderIngestMergeRowsStacksWhenNarrow(t *testing.T) {
	iv := sflog.IngestView{
		ShowMerge: true, Unique: 5, Skipped: 120, BucketsDone: 96, BucketsTotal: 128,
	}
	const inner = 28
	// Force stack: narrower than the single-line Merge content.
	rows := renderIngestMergeRows(iv, inner)
	if len(rows) < 2 {
		t.Fatalf("narrow Merge should stack, got %d rows:\n%v", len(rows), rows)
	}
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"Merge", "added", "already in library", "bucket", "96", "128"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("stacked Merge missing %q:\n%s", want, joined)
		}
	}
	for i, row := range rows {
		if w := lipgloss.Width(row); w > inner {
			t.Fatalf("stacked row %d width %d exceeds inner %d: %q", i, w, inner, row)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("stacked Merge must not rely on ellipsis:\n%s", joined)
	}
	// Sanity: the single-line form would be mid-cut by padOrTrim at this width.
	single := renderIngestMergeRows(iv, 200)[0]
	if trimmed := sflPadOrTrim(single, inner); !strings.Contains(trimmed, "…") {
		t.Fatalf("sanity: padOrTrim should ellipsize oversized Merge; got %q", trimmed)
	}
}

// TestRenderIngestMergeRowsSurviveGradientBox is the Bugbot regression: stacked
// continuations used to keep a full label indent, still overflow boxInner, and
// get ellipsized by sflPadOrTrim inside sflGradientBox — after the user-visible
// renderer, not just the pre-box strings.
func TestRenderIngestMergeRowsSurviveGradientBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	iv := sflog.IngestView{
		ShowMerge: true, Unique: 5, Skipped: 120, BucketsDone: 96, BucketsTotal: 128,
	}
	// width 42 => boxInner 28 (same budget the live ingest frame uses).
	const width = 42
	inner := boxInner(width)
	if inner != 28 {
		t.Fatalf("boxInner(%d) = %d, want 28 (test assumption)", width, inner)
	}
	body := append([]string{gradientBar(0.5, inner)}, renderIngestMergeRows(iv, inner)...)
	joined := strings.Join(sflGradientBox(body, width, gradStart, gradEnd), "\n")
	for _, want := range []string{"added", "already in library", "bucket", "96", "128"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("boxed Merge missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("boxed Merge must not be ellipsized by padOrTrim:\n%s", joined)
	}
}

func TestRenderSflRemovedRowsSurviveGradientBoxWithShares(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// width 56 => boxInner 42 — fits share-annotated "already in library".
	const width = 56
	inner := boxInner(width)
	bullets := []string{
		sflWarnStyle.Render("2") + sflMutedStyle.Render(tuistat.ShareParen(2, 10)) + " " + sflMutedStyle.Render("rejected"),
		sflCountStyle.Render("3") + sflMutedStyle.Render(tuistat.ShareParen(3, 10)) + " " + sflMutedStyle.Render("already in library"),
	}
	rows := renderSflRemovedRows(bullets, inner)
	if len(rows) < 2 {
		t.Fatalf("expected stacked Removed rows, got %d", len(rows))
	}
	for i, row := range rows {
		if w := lipgloss.Width(row); w > inner {
			t.Fatalf("pre-box row %d width %d > %d", i, w, inner)
		}
	}
	joined := strings.Join(sflGradientBox(rows, width, gradStart, gradEnd), "\n")
	for _, want := range []string{"rejected", "already in library", "(20.0%)", "(30.0%)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("boxed Removed missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("boxed Removed must not be ellipsized:\n%s", joined)
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

// ingest regen rows must share one bar column even when archive names differ
// in length (and when only some rows have a part annotation). Without a fixed
// left column the INGESTING worker bars stair-step, which is what users saw
// rebuilding a library whose part names are not uniform.
func TestRenderIngestRegenRowsAlignBarsAcrossNameLengths(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	panel := renderIngestRegenPanel([]sflog.IngestWorker{
		{Archive: "a.zst", BytesDone: 50, BytesTotal: 100},
		{Archive: "/data/lib/sfu_20260101_120000_part04.txt.zst", PartIdx: 4, PartsTotal: 16, BytesDone: 50, BytesTotal: 100},
		{Archive: "mid_name.txt.zst", PartIdx: 1, PartsTotal: 2, BytesDone: 1 << 30, BytesTotal: 2 << 30},
	}, 80, 0)
	var rows []string
	for _, ln := range panel {
		if ingestWorkerBarCol(ln) >= 0 {
			rows = append(rows, ln)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 worker rows with bars, got %d:\n%s", len(rows), strings.Join(panel, "\n"))
	}
	want := ingestWorkerBarCol(rows[0])
	if want < 0 {
		t.Fatalf("row 0 has no bar: %q", stripANSI(rows[0]))
	}
	for i, ln := range rows[1:] {
		if got := ingestWorkerBarCol(ln); got != want {
			t.Errorf("row %d bar at col %d; want %d\nrow 0: %q\nrow %d: %q",
				i+1, got, want, stripANSI(rows[0]), i+1, stripANSI(ln))
		}
	}
	for i, ln := range rows {
		if w := lipgloss.Width(ln); w > 80 {
			t.Errorf("row %d visible width %d > inner 80: %q", i, w, stripANSI(ln))
		}
	}
}

// boxed regen rows must never exceed inner: sflPadOrTrim would chop the bar
// and bytes off the right and break the shared column. Drop the bytes
// column, then shrink the bar, before overflowing.
func TestRenderIngestRegenRowsFitNarrowInner(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	workers := []sflog.IngestWorker{
		{Archive: "a.zst", BytesDone: 50, BytesTotal: 100},
		{Archive: "/data/lib/sfu_20260101_120000_part04.txt.zst", PartIdx: 4, PartsTotal: 16, BytesDone: 50, BytesTotal: 100},
	}
	for _, inner := range []int{24, 40, 48, 66} {
		panel := renderIngestRegenPanel(workers, inner, 0)
		var rows []string
		var barCol = -1
		for _, ln := range panel {
			if col := ingestWorkerBarCol(ln); col >= 0 {
				if w := lipgloss.Width(ln); w > inner {
					t.Errorf("inner %d: row width %d > inner: %q", inner, w, stripANSI(ln))
				}
				if barCol < 0 {
					barCol = col
				} else if col != barCol {
					t.Errorf("inner %d: bar col %d vs %d\n%s", inner, col, barCol, stripANSI(ln))
				}
				rows = append(rows, ln)
			}
		}
		if len(rows) != 2 {
			t.Errorf("inner %d: want 2 bar rows, got %d\n%s", inner, len(rows), strings.Join(panel, "\n"))
		}
	}
	wide := strings.Join(renderIngestRegenPanel(workers, 80, 0), "\n")
	if !strings.Contains(stripANSI(wide), "/") {
		t.Fatalf("wide inner should still show the bytes column:\n%s", stripANSI(wide))
	}
}

func ingestWorkerBarCol(line string) int {
	plain := stripANSI(line)
	idx := strings.IndexAny(plain, "█▆░")
	if idx < 0 {
		return -1
	}
	return lipgloss.Width(plain[:idx])
}

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

// TestSflPlainExtractFrameFitsTerminal is the plain-frame counterpart of the
// -secrets flicker guard: the single-bar plain frame height is exactly
// sflPlainFrameOverhead + worker rows and never exceeds termHeight-1.
func TestSflPlainExtractFrameFitsTerminal(t *testing.T) {
	for h := 16; h <= 80; h++ {
		rows := sflPlainWorkerRows(h, 16)
		if rows == 0 {
			continue
		}
		frameHeight := sflPlainFrameOverhead + rows
		if frameHeight > h-1 {
			t.Fatalf("termHeight=%d: frame height %d exceeds clamp %d (footer would truncate)",
				h, frameHeight, h-1)
		}
	}
}

// TestSflPlainExtractFrameStacked locks the unified plain extract frame to the
// sfu-style stacked layout: a stats box, the Extract bar on its own line below
// it, and the worker panel in its own box — and NO Secrets bar / Secrets row,
// which are -secrets-only. It builds the frame from the same helpers the plain
// branch of renderProgress composes, so a wiring regression here is caught
// without needing to run the engine to set Progress.Total.
func TestSflPlainExtractFrameStacked(t *testing.T) {
	prog := sflog.NewProgress()
	prog.SetWorkers(4)
	const width = 80
	inner := boxInner(width)
	header := headerLine(sflSpinnerStyle.Render("·"), sflOkStyle.Render("[sfl] EXTRACTING"), 0, width)
	statRows := renderExtractStatsRows(1, 1, 1, 10, 5, 2, 1<<19, 1<<20, 1e6)
	plainBox := sflGradientBox(statRows, width, gradStart, gradEnd)
	plainBars := []string{sflIndent + sflBarLabel("Extract") + gradientBar(prog.Fraction(), sflBarBody(width))}
	panel := sflWorkerPanelBox(prog, width, inner, 0, sflPlainFrameOverhead)
	lines := sflFrameWithBars(header, plainBox, plainBars, panel, width)
	joined := strings.Join(lines, "\n")

	if strings.Contains(joined, "Secrets") {
		t.Fatalf("plain frame leaked Secrets content (must be -secrets-only):\n%s", joined)
	}
	if !strings.Contains(joined, sflBarLabel("Extract")) {
		t.Fatalf("plain frame missing the Extract bar:\n%s", joined)
	}
	// Two gradient boxes: the stats box and the worker box. Each has one ╭ and
	// one ╰, so the stacked layout (bar between them, worker panel separate) is
	// present. The old single-box layout had only one of each.
	topBorders := strings.Count(joined, "╭")
	botBorders := strings.Count(joined, "╰")
	if topBorders != 2 || botBorders != 2 {
		t.Fatalf("plain frame want 2 gradient boxes (stats + worker), got %d top / %d bottom borders:\n%s",
			topBorders, botBorders, joined)
	}
	// The Extract bar sits between the two boxes: the first ╰ (stats box close)
	// must come before the Extract bar, which must come before the second ╭
	// (worker box open).
	statsClose := strings.Index(joined, "╰")
	extractAt := strings.Index(joined, sflBarLabel("Extract"))
	workerOpen := statsClose + strings.Index(joined[statsClose:], "╭")
	if !(statsClose < extractAt && extractAt < workerOpen) {
		t.Fatalf("Extract bar not between stats box and worker box (statsClose=%d extract=%d workerOpen=%d):\n%s",
			statsClose, extractAt, workerOpen, joined)
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
	// ...but the encrypted/password signal lives ONLY in the dedicated warning
	// box printed above the summary (see TestRenderEncryptedWarning), so the
	// summary box itself must not duplicate it as an in-box row.
	if strings.Contains(joined, "no password matched") {
		t.Fatalf("summary box must not duplicate the encrypted signal:\n%s", joined)
	}
	// ...and per-issue detail is streamed to the -err file, never stdout.
	for _, absent := range []string{"passwords not found", "locked.zip"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("issue detail %q must not appear on stdout:\n%s", absent, joined)
		}
	}
}

// TestRenderEncryptedWarning tailors the remediation hint to whether -p was
// supplied, lists the affected archives, and is suppressed when nothing was
// undecryptable. The dedicated box is what makes a "0 ULP" encrypted run
// self-diagnosing instead of reading as an empty archive.
func TestRenderEncryptedWarning(t *testing.T) {
	stats := func(provided bool) sflog.ExtractStats {
		return sflog.ExtractStats{
			PasswordNotFound: 2,
			Issues: []sflog.Issue{
				{Path: "/data/a.zip", Kind: sflog.IssuePasswordNotFound},
				{Path: "/data/b.zip", Kind: sflog.IssuePasswordNotFound},
			},
		}
	}
	// No -p: tell the user to provide one.
	noP := strings.Join(renderEncryptedWarning(stats(false), false, 100), "\n")
	if !strings.Contains(noP, "ENCRYPTED") || !strings.Contains(noP, "No password was supplied") {
		t.Fatalf("no-(-p) warning missing title/supplied note:\n%s", noP)
	}
	if !strings.Contains(noP, "sfl -p") {
		t.Fatalf("no-(-p) warning missing -p hint:\n%s", noP)
	}
	if !strings.Contains(noP, "a.zip") || !strings.Contains(noP, "b.zip") {
		t.Fatalf("warning missing affected archive paths:\n%s", noP)
	}
	// With -p: don't say "use -p" (they did); point at the list instead.
	withP := strings.Join(renderEncryptedWarning(stats(true), true, 100), "\n")
	if strings.Contains(withP, "sfl -p") || strings.Contains(withP, "No password was supplied") {
		t.Fatalf("with-(-p) warning must not re-tell the user to use -p:\n%s", withP)
	}
	if !strings.Contains(withP, "supplied password(s) opened") || !strings.Contains(withP, "Verify the list") {
		t.Fatalf("with-(-p) warning missing list-verification hint:\n%s", withP)
	}
	// Suppressed when nothing was undecryptable.
	if got := renderEncryptedWarning(sflog.ExtractStats{}, false, 100); len(got) != 0 {
		t.Fatalf("warning must be nil when PasswordNotFound=0, got %v", got)
	}
	// "+N more" when the capped issue list is shorter than the total.
	capped := sflog.ExtractStats{PasswordNotFound: 5, Issues: []sflog.Issue{{Path: "/x.zip", Kind: sflog.IssuePasswordNotFound}}}
	more := strings.Join(renderEncryptedWarning(capped, false, 100), "\n")
	if !strings.Contains(more, "4 more not shown") {
		t.Fatalf("warning missing '+N more' note:\n%s", more)
	}
}

func TestRenderEncryptedWarningKeepsPathTail(t *testing.T) {
	longPath := "/run/media/bigboi/b992e755-aaaa-bbbb-cccc-dddddddddddd/Data_logs/deep/nested/locked_release.zip"
	stats := sflog.ExtractStats{
		PasswordNotFound: 1,
		Issues:           []sflog.Issue{{Path: longPath, Kind: sflog.IssuePasswordNotFound}},
	}
	// Tight width forces TruncatePath inside the warning box.
	joined := strings.Join(renderEncryptedWarning(stats, false, 40), "\n")
	if !strings.Contains(joined, "locked_release.zip") {
		t.Fatalf("encrypted warning must keep basename tail:\n%s", joined)
	}
	if strings.Contains(joined, "b992e755") {
		t.Fatalf("head UUID should be truncated away at narrow width:\n%s", joined)
	}
}

func TestRenderIngestOutputFooter(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	lines := renderIngestOutputFooter([]string{
		"/lib/sfu_20260701_part1.txt.zst",
		"/lib/sfu_20260701_part2.txt.zst",
	}, false)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Output", "part1.txt.zst", "part2.txt.zst"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("footer missing %q:\n%s", want, joined)
		}
	}
}

// longUUIDLibraryPath mimics a removable-volume mount that used to get
// head-truncated inside the gradient box ("…/D" instead of the useful basename).
const longUUIDLibraryPath = "/run/media/bigboi/b992e755-aaaa-bbbb-cccc-dddddddddddd/Data_logs"

func TestRenderIngestSummaryLongLibraryPathOutsideBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	lines := renderIngestSummary(longUUIDLibraryPath, 1234, 5, 3, 2, sflog.ExtractStats{
		Logs: 2, Credentials: 10, Emitted: 10,
	}, []string{longUUIDLibraryPath + "/sfu_part1.txt.zst"}, false)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, longUUIDLibraryPath) {
		t.Fatalf("library path must appear in full:\n%s", joined)
	}
	// Ellipsis on the library path itself is the failure mode; Output footer
	// may still list the same prefix as a full path (no ellipsis).
	closeIdx := strings.LastIndex(joined, "╰")
	libIdx := strings.Index(joined, longUUIDLibraryPath)
	if closeIdx < 0 || libIdx < 0 || libIdx < closeIdx {
		t.Fatalf("library path should be below the gradient box:\n%s", joined)
	}
	pathLine := joined[libIdx:]
	if i := strings.IndexByte(pathLine, '\n'); i >= 0 {
		pathLine = pathLine[:i]
	}
	if strings.Contains(pathLine, "…") {
		t.Fatalf("library path line must not be ellipsized: %q", pathLine)
	}
}

func TestRenderNoIngestSummaryLongLibraryPathOutsideBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	lines := renderNoIngestSummary(longUUIDLibraryPath, sflog.ExtractStats{Logs: 1}, false)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, longUUIDLibraryPath) {
		t.Fatalf("library path must appear in full:\n%s", joined)
	}
	closeIdx := strings.LastIndex(joined, "╰")
	libIdx := strings.Index(joined, longUUIDLibraryPath)
	if closeIdx < 0 || libIdx < closeIdx {
		t.Fatalf("library path should be below the COMPLETE frame:\n%s", joined)
	}
	if strings.Contains(joined, "Library:") {
		t.Fatalf("in-box 'Library:' label should be gone (footer uses Library gutter):\n%s", joined)
	}
}

func TestRenderFinalSummaryLongOutputPathOutsideBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	outPath := longUUIDLibraryPath + "/sfl_20260812_120000.txt"
	lines := renderFinalSummary(outPath, sflog.ExtractStats{Emitted: 1})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, outPath) {
		t.Fatalf("output path must appear in full:\n%s", joined)
	}
	closeIdx := strings.LastIndex(joined, "╰")
	pathIdx := strings.Index(joined, outPath)
	if closeIdx < 0 || pathIdx < closeIdx {
		t.Fatalf("output path should be below the COMPLETE frame:\n%s", joined)
	}
	if strings.Contains(joined, "Output:") {
		t.Fatalf("in-box 'Output:' label should be gone (footer uses Output gutter):\n%s", joined)
	}
	if !strings.Contains(joined, "┃") {
		t.Fatalf("missing path footer gutter:\n%s", joined)
	}
}

func TestEnvPathFooterSplicedWhenCopied(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	envDir := longUUIDLibraryPath + "/sfl_20260812_120000_secrets"
	stats := sflog.ExtractStats{Emitted: 1, EnvCopied: 3}
	summary := renderFinalSummaryWithNotice("out/sfl.txt", stats, nil)
	frost := summaryFooterLines(termWidth(), nil)
	summary = spliceBeforeFooter(summary,
		renderSflPathFooter("Env      ", []string{envDir}, sflMutedStyle), frost)

	joined := strings.Join(summary, "\n")
	if !strings.Contains(joined, envDir) {
		t.Fatalf("env path must appear in full:\n%s", joined)
	}
	if !strings.Contains(joined, "Env") || !strings.Contains(joined, "┃") {
		t.Fatalf("missing Env path footer gutter:\n%s", joined)
	}
	closeIdx := strings.LastIndex(joined, "╰")
	envIdx := strings.Index(joined, envDir)
	if closeIdx < 0 || envIdx < closeIdx {
		t.Fatalf("env path should be below the COMPLETE frame:\n%s", joined)
	}
	if strings.Contains(joined, "Env:") {
		t.Fatalf("in-box 'Env:' label must not return:\n%s", joined)
	}
	if !strings.Contains(joined, "Env files") || !strings.Contains(joined, "copied") {
		t.Fatalf("recap should still show Env files count:\n%s", joined)
	}
}

func TestEnvPathFooterAbsentWhenNothingCopied(t *testing.T) {
	joined := strings.Join(renderFinalSummary("out/sfl.txt", sflog.ExtractStats{Emitted: 1}), "\n")
	// No splice when EnvCopied==0 && EnvDirsCopied==0 (mirrors main).
	if strings.Contains(joined, "Env      ") {
		t.Fatalf("no Env footer when nothing copied:\n%s", joined)
	}
}

func TestEnvPathFooterSplicedWhenOnlyTdataCopied(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	envDir := longUUIDLibraryPath + "/sfl_20260812_120000_secrets"
	stats := sflog.ExtractStats{Emitted: 1, EnvDirsCopied: 1}
	summary := renderFinalSummaryWithNotice("out/sfl.txt", stats, nil)
	frost := summaryFooterLines(termWidth(), nil)
	summary = spliceBeforeFooter(summary,
		renderSflPathFooter("Env      ", []string{envDir}, sflMutedStyle), frost)

	joined := strings.Join(summary, "\n")
	if !strings.Contains(joined, envDir) {
		t.Fatalf("env path must appear when only tdata folders copied:\n%s", joined)
	}
	if !strings.Contains(joined, "tdata") {
		t.Fatalf("recap should show tdata row:\n%s", joined)
	}
	if strings.Contains(joined, "Env files") {
		t.Fatalf("EnvCopied==0 should omit Env files row:\n%s", joined)
	}
}

func TestRenderSecretsBlockLongStorePathOutsideBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	dbPath := longUUIDLibraryPath + "/sfl-secrets.sqlite"
	lines := renderSecretsBlock(secrets.Stats{New: 2, Existing: 1}, dbPath, 80)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, dbPath) {
		t.Fatalf("store path must appear in full:\n%s", joined)
	}
	closeIdx := strings.LastIndex(joined, "╰")
	pathIdx := strings.Index(joined, dbPath)
	if closeIdx < 0 || pathIdx < closeIdx {
		t.Fatalf("store path should be below the secrets box:\n%s", joined)
	}
	if strings.Contains(joined, "…") && strings.Contains(joined[pathIdx:pathIdx+len(dbPath)+2], "…") {
		t.Fatalf("store path must not be ellipsized:\n%s", joined)
	}
}

// TestRenderIngestOutputFooterNothingNew proves a completed ingest that added
// nothing (all duplicates -> engine discarded the empty shard -> empty paths)
// states "(nothing new)" instead of silently dropping the Output row.
func TestRenderIngestOutputFooterNothingNew(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	joined := strings.Join(renderIngestOutputFooter(nil, false), "\n")
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

// -odr dry-run: ingest summary title becomes "SnowFastLog DRY RUN", the
// Added row is relabeled "Would add", and the output footer states nothing
// was written instead of listing (temp, already-cleaned) part paths.
func TestRenderIngestSummaryDryRun(t *testing.T) {
	lines := renderIngestSummary("/data/Library", 1234, 5, 3, 2, sflog.ExtractStats{
		Logs:        2,
		Credentials: 10,
		Emitted:     10,
	}, []string{"/data/Library/sfu_20260701_part1.txt.zst"}, true)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"DRY RUN",
		"Would add",
		"(dry run — nothing written)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dry-run ingest summary missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "INGESTED") {
		t.Errorf("dry-run title should be DRY RUN, not INGESTED:\n%s", joined)
	}
	// the part path must NOT be surfaced in a dry-run footer
	if strings.Contains(joined, "sfu_20260701_part1.txt.zst") {
		t.Errorf("dry-run footer must not list the would-be part path:\n%s", joined)
	}
}

// dry-run with nothing extracted still flags DRY RUN in the title.
func TestRenderNoIngestSummaryDryRun(t *testing.T) {
	lines := renderNoIngestSummary("/data/Library", sflog.ExtractStats{
		Logs: 1, ArchivesScanned: 1, SkippedArchives: 1, PasswordNotFound: 1,
	}, true)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "DRY RUN") {
		t.Errorf("dry-run no-ingest summary missing DRY RUN title:\n%s", joined)
	}
	if strings.Contains(joined, "SnowFastLog COMPLETE") {
		t.Errorf("dry-run no-ingest title should be DRY RUN, not COMPLETE:\n%s", joined)
	}
}

// the live header carries an amber DRY RUN marker when the run is a preview.
func TestRenderProgressDryRunHeader(t *testing.T) {
	prog := sflog.NewProgress()
	prog.SetDryRun(true)
	joined := strings.Join(renderProgress(0, prog, 0, 0, 0, 80), "\n")
	if !strings.Contains(joined, "DRY RUN") {
		t.Fatalf("dry-run live header missing DRY RUN marker:\n%s", joined)
	}
}

// -od/-odr must surface sfu's "vs library" hint on the live header during
// extract and ingest, including a compact key count once ingest knows it.
func TestRenderProgressLibraryHeaderBadge(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	plain := strings.Join(renderProgress(0, sflog.NewProgress(), 0, 0, 0, 80), "\n")
	if strings.Contains(plain, "vs library") {
		t.Fatalf("non-od run must not show a library badge:\n%s", plain)
	}

	prog := sflog.NewProgress()
	prog.SetLibrary(true)
	extract := strings.Join(renderProgress(0, prog, 0, 0, 0, 80), "\n")
	if !strings.Contains(extract, "vs library") {
		t.Fatalf("od extract header missing vs library:\n%s", extract)
	}

	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{LibraryKeys: 3_290_076_168}
	})
	ingest := strings.Join(renderProgress(0, prog, 0, 0, 0, 80), "\n")
	if !strings.Contains(ingest, "INGESTING") {
		t.Fatalf("ingest frame missing INGESTING:\n%s", ingest)
	}
	if !strings.Contains(ingest, "vs 3.29B library") {
		t.Fatalf("od ingest header missing vs N library:\n%s", ingest)
	}
}

func TestLibraryBadgeDroppedOnNarrowTerminal(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.SetLibrary(true)
	base := sflOkStyle.Render("[sfl] EXTRACTING")
	if got := libraryBadge(prog, sflog.IngestView{}, base, 110); !strings.Contains(got, "vs library") {
		t.Fatalf("wide terminal should show vs library, got %q", got)
	}
	if got := libraryBadge(prog, sflog.IngestView{}, base, 40); got != "" {
		t.Fatalf("narrow terminal should drop the library badge, got %q", got)
	}
}
