package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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
		// No outcome title (COMPLETE/✓/⚠) anymore: the summary leads with the
		// recap box; outcome state lives in the exit code and issues log.
		"Input",
		"2 archives",
		"Lines",
		"10 parsed",
		"Unique",
		"(80.0%)",
		"entries",
		"Removed",
		"dup",
		"out/sfl.txt",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("summary missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "4 files") {
		t.Fatalf("summary must not render a files segment:\n%s", joined)
	}
}

func TestRecapCountRowsUniqueSharesOmitWhenWhole(t *testing.T) {
	joined := strings.Join(recapCountRows(sflog.ExtractStats{
		Credentials: 8, Emitted: 8, Duplicates: 0,
	}, 8, nil, nil), "\n")
	if strings.Contains(joined, "%") {
		t.Fatalf("all-unique recap must omit 100%% share:\n%s", joined)
	}
}

// Unique share on one row at the default 80-col box (boxInner 66); the
// duplicate count rides sfu-style Removed bullets, never a mid-cut tail.
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
	joined := strings.Join(sflGradientBox(recapCountRows(stats, int64(stats.Emitted),
		ingestRemovedBullets(0, 0, int64(stats.Duplicates), int64(stats.Credentials)), nil), width, gradStart, gradEnd), "\n")
	for _, want := range []string{"Unique", "(50.0%)", "Removed", "dup", "1,000,000,000"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("boxed Unique missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Fatalf("boxed Unique must not be ellipsized by padOrTrim:\n%s", joined)
	}
}

func TestIngestRemovedBulletsSharesVsParsed(t *testing.T) {
	joined := strings.Join(renderSflRemovedRows(ingestRemovedBullets(2, 3, 5, 10), 72), "\n")
	for _, want := range []string{"(50.0%)", "(30.0%)", "(20.0%)", "dup", "rejected", "already in library"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ingest library rows missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderEnvLiveRow(t *testing.T) {
	row := renderEnvLiveRow(12, 0)
	if !strings.Contains(row, "12") || !strings.Contains(row, "copied") {
		t.Fatalf("unexpected env live row: %q", row)
	}
	if strings.Contains(row, "dupes") {
		t.Fatalf("zero dedup must omit the dedup tail: %q", row)
	}
	dedup := renderEnvLiveRow(12, 4)
	if !strings.Contains(dedup, "12") || !strings.Contains(dedup, "copied") ||
		!strings.Contains(dedup, "4") || !strings.Contains(dedup, "dupes") {
		t.Fatalf("dedup tail missing from live env row: %q", dedup)
	}
}

// TestRenderEnvLiveRowIgnoresTdataTrees pins the rationalized counter: copied
// tdata trees must not inflate the live Env row. creditTdataDir credits only
// stats.DirsCopied, so a copier that promoted 100-file tdata trees alongside a
// single flat env copy leaves the live row at "1 copied" (the recap shows the
// trees as a "N tdata folders" tail instead).
func TestRenderEnvLiveRowIgnoresTdataTrees(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	prog := sflog.NewProgress()
	prog.EnableEnv()
	copier := sflog.NewEnvCopier(root, prog, sflog.EnvCopyMaxLen)
	copier.Start()
	flatA := filepath.Join(t.TempDir(), "config.env")
	flatB := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(flatA, []byte("API_KEY=a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flatB, []byte("SECRET=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	copier.EnqueueFile(flatA)
	copier.EnqueueFile(flatB)
	tree := filepath.Join(t.TempDir(), "tdata-src")
	if err := os.MkdirAll(filepath.Join(tree, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		if err := os.WriteFile(filepath.Join(tree, "cache", fmt.Sprintf("f%03d", i)),
			[]byte("blob"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := copier.CopyDir(tree); err != nil {
		t.Fatal(err)
	}
	es := copier.Close()
	if es.Copied != 2 || es.DirsCopied != 1 {
		t.Fatalf("copied = %d dirs = %d, want 2/1", es.Copied, es.DirsCopied)
	}
	if got := prog.EnvCopied(); got != 2 {
		t.Fatalf("live env copied = %d, want 2 (tdata tree files must not count)", got)
	}
	row := renderEnvLiveRow(prog.EnvCopied(), prog.EnvDeduped())
	if !strings.Contains(row, "2 copied") || strings.Contains(row, "102") {
		t.Fatalf("live env row must show flat copies only: %q", row)
	}
}

func TestRecapCountRowsIncludesEnv(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvCopied: 3}, 0, nil, nil)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "Env") || !strings.Contains(joined, "3") || !strings.Contains(joined, "copied") {
		t.Fatalf("recap missing env row:\n%s", joined)
	}
	if strings.Contains(joined, "context") {
		t.Fatalf("recap leaked internal context wording:\n%s", joined)
	}
	// A nonzero dedup count rides the same row as a " · N dupes" tail.
	dedup := strings.Join(recapCountRows(sflog.ExtractStats{EnvCopied: 3, EnvDeduped: 2}, 0, nil, nil), "\n")
	if !strings.Contains(dedup, "copied") || !strings.Contains(dedup, "2") || !strings.Contains(dedup, "dupes") {
		t.Fatalf("recap missing dedup tail:\n%s", dedup)
	}
	// tdata folders fold into the Env row as a tail (one row fewer than a
	// separate tdata row); a tdata-only run still shows the row.
	baseline := recapCountRows(sflog.ExtractStats{EnvCopied: 3}, 0, nil, nil)
	folded := recapCountRows(sflog.ExtractStats{EnvCopied: 3, EnvDeduped: 2, EnvDirsCopied: 5743}, 0, nil, nil)
	if len(folded) != len(baseline) {
		t.Fatalf("tdata tail must ride the Env row, not add one: baseline %d rows, folded %d rows:\n%s",
			len(baseline), len(folded), strings.Join(folded, "\n"))
	}
	if !strings.Contains(folded[len(folded)-1], "3") || !strings.Contains(folded[len(folded)-1], "dupes") ||
		!strings.Contains(folded[len(folded)-1], "5,743 tdata folders") {
		t.Fatalf("recap missing folded tdata tail:\n%s", folded[len(folded)-1])
	}
	onlyDirs := strings.Join(recapCountRows(sflog.ExtractStats{EnvDirsCopied: 2}, 0, nil, nil), "\n")
	if !strings.Contains(onlyDirs, "0 copied") || !strings.Contains(onlyDirs, "2 tdata folders") {
		t.Fatalf("tdata-only recap must show the Env row with 0 copied:\n%s", onlyDirs)
	}
}

func TestRecapCountRowsIncludesTdata(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvDirsCopied: 2}, 0, nil, nil)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "Env") || !strings.Contains(joined, "2 tdata folders") {
		t.Fatalf("recap missing tdata tail on the Env row:\n%s", joined)
	}
	if strings.Contains(joined, "folder(s)") || strings.Contains(joined, "1 tdata folders") {
		t.Fatalf("tdata tail must use plural.Noun forms:\n%s", joined)
	}
	single := strings.Join(recapCountRows(sflog.ExtractStats{EnvDirsCopied: 1}, 0, nil, nil), "\n")
	if !strings.Contains(single, "1 tdata folder") || strings.Contains(single, "folder(s)") {
		t.Fatalf("tdata tail must render singular form:\n%s", single)
	}
}

func TestRecapCountRowsIncludesOverCap(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvSkippedOverCap: 2}, 0, nil, nil)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "Envs skip") || !strings.Contains(joined, "over 16 MiB cap") || !strings.Contains(joined, "2") {
		t.Fatalf("recap missing env over-cap row:\n%s", joined)
	}
	if strings.Contains(joined, "tdata skip") {
		t.Fatalf("flat env over-cap must not use tdata skip row:\n%s", joined)
	}
}

func TestRecapCountRowsIncludesTdataOverCap(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{EnvDirsSkippedOverCap: 3}, 0, nil, nil)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "tdata skip") || !strings.Contains(joined, "over 5 GiB cap") || !strings.Contains(joined, "3") {
		t.Fatalf("recap missing tdata over-cap row:\n%s", joined)
	}
	if strings.Contains(joined, "Envs skip") {
		t.Fatalf("tdata over-cap must not use Env skip row:\n%s", joined)
	}
}

// F3 (#6): an all-skipped -history rerun never extracts, so the extraction
// counters would read 0 and the recap would contradict the "discovering
// sources… 2 found" frame and the History row. main rolls the skipped hits
// into the scanned totals before rendering; with results carrying those hits,
// the Input row must show the sources discovery actually found.
func TestRecapInputRowShowsHistorySkippedSources(t *testing.T) {
	hits := []sflog.SourceResult{
		{Path: "/in/a.zip", IsArchive: true, OK: true, HistoryComplete: true, HistoryHit: true},
		{Path: "/in/b.zip", IsArchive: true, OK: true, HistoryComplete: true, HistoryHit: true},
	}
	rows := recapCountRows(sflog.ExtractStats{HistoryChecked: 2, HistorySkipped: 2}, 0, nil, hits)
	joined := stripANSI(strings.Join(rows, "\n"))
	for _, want := range []string{"2 archives"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("all-skipped recap Input row missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "nested") || strings.Contains(joined, "files") {
		t.Fatalf("all-skipped recap must not show nested or files segments:\n%s", joined)
	}
	if !strings.Contains(joined, "2 checked") || !strings.Contains(joined, "2 skipped") {
		t.Fatalf("all-skipped recap missing the History row:\n%s", joined)
	}
}

// F4 (#7): "Input 206 archives" conflated the 2 archives the user passed with
// 204 discovered inside them. With the run's results, the Input row splits
// top-level from nested (2026-10-01). Loose file sources are not rendered —
// the files segment was removed entirely (2026-10-03, user decision).
func TestRecapInputRowSplitsNestedArchives(t *testing.T) {
	processed := []sflog.SourceResult{
		{Path: "/in/#4255.zip", IsArchive: true, OK: true},
		{Path: "/in/#4256.zip", IsArchive: true, OK: true},
		{Path: "/in/loose.txt", OK: true},
	}
	rows := recapCountRows(sflog.ExtractStats{
		ArchivesScanned: 206, FilesScanned: 681, Credentials: 100, Emitted: 90,
	}, 90, nil, processed)
	joined := stripANSI(strings.Join(rows, "\n"))
	for _, want := range []string{"2 archives", "204 nested"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("nested-split recap Input row missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "206") || strings.Contains(joined, "681") {
		t.Fatalf("recap must lead with the work-item counts, not the folded totals:\n%s", joined)
	}
	if strings.Contains(joined, "file") {
		t.Fatalf("recap must not render a files segment:\n%s", joined)
	}
}

// F4 (#7): counts of one use the singular forms across the Input row, and a
// files-only run shows no nested segment.
func TestRecapInputRowNestedAndSingularForms(t *testing.T) {
	rows := recapCountRows(sflog.ExtractStats{ArchivesScanned: 2, FilesScanned: 3}, 0, nil,
		[]sflog.SourceResult{{Path: "/in/a.zip", IsArchive: true, OK: true}})
	joined := stripANSI(strings.Join(rows, "\n"))
	for _, want := range []string{"1 archive ", "1 nested"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("singular nested recap missing %q:\n%s", want, joined)
		}
	}
	filesOnly := stripANSI(strings.Join(recapCountRows(sflog.ExtractStats{FilesScanned: 2}, 0, nil,
		[]sflog.SourceResult{{Path: "/in/p.txt", OK: true}}), "\n"))
	if strings.Contains(filesOnly, "nested") || strings.Contains(filesOnly, "file") {
		t.Fatalf("files-only recap must not show nested or files segments:\n%s", filesOnly)
	}
}

// F4 (#14): the recap's Unique and the ingest Added rows must not hardcode
// "entries" — counts of one read "1 entry".
func TestRecapUniqueEntrySingular(t *testing.T) {
	joined := stripANSI(strings.Join(recapCountRows(sflog.ExtractStats{Credentials: 1}, 1, nil, nil), "\n"))
	if strings.Contains(joined, "entries") || !strings.Contains(joined, "1 entry") {
		t.Fatalf("Unique row must use plural.Noun for entries:\n%s", joined)
	}
	rows := renderIngestLibraryRows(1, 0, 0, 1, 66, false)
	added := stripANSI(strings.Join(rows, "\n"))
	if strings.Contains(added, "entries") || !strings.Contains(added, "1 entry") {
		t.Fatalf("ingest Added row must use plural.Noun for entries:\n%s", added)
	}
}

// F4 (#7) live anchor: once processing outpaces discovery (nested members at
// work), the Sources row anchors with the discovered top-level count — the
// same number the SCANNING frame reported — so the climbing archive count
// never reads as what the user passed.
func TestRenderExtractStatsRowsAnchorsDiscoveredSources(t *testing.T) {
	joined := stripANSI(strings.Join(renderExtractStatsRows(677, 206, 2, 2, 2, 5, 2, 1<<19, 1<<20, 1e6), "\n"))
	if !strings.Contains(joined, "2 found") || !strings.Contains(joined, "677 files") {
		t.Fatalf("live Sources row missing the discovered anchor:\n%s", joined)
	}
	if strings.Contains(joined, "logs") || strings.Contains(joined, "log") {
		t.Fatalf("live Sources row must not show the logs counter:\n%s", joined)
	}
	// Discovery == processing (loose files, nothing nested): no anchor noise.
	clean := stripANSI(strings.Join(renderExtractStatsRows(3, 0, 3, 1, 1, 5, 2, 1<<19, 1<<20, 1e6), "\n"))
	if strings.Contains(clean, "found") {
		t.Fatalf("Sources row must not anchor when processing matches discovery:\n%s", clean)
	}
}

func TestRenderIngestSummaryHasNoOutcomeTitleOutsideDryRun(t *testing.T) {
	stats := sflog.ExtractStats{Logs: 2, Credentials: 10, Emitted: 10}
	lines := renderIngestSummaryWithNotice("/data/Library", 1234, 5, 3, 2, stats,
		[]string{"/data/Library/sfu_20260701_part1.txt.zst"}, nil, false, nil)
	joined := strings.Join(lines, "\n")
	// Non-dry-run summaries are fully silent on top: no title, no ✓/⚠ mark,
	// and no history-note surface (state lives in exit code 3 + issues log).
	for _, absent := range []string{
		"INGESTED", "COMPLETE", "✓", "⚠",
		"HISTORY NOT RECORDED", "not recorded", "sources retained",
	} {
		if strings.Contains(joined, absent) {
			t.Fatalf("ingest summary must not carry outcome title/note %q:\n%s", absent, joined)
		}
	}
	// Dry-run frames keep the DRY RUN mode indicator.
	dry := strings.Join(renderIngestSummary("/data/Library", 1234, 5, 3, 2, stats,
		[]string{"/data/Library/sfu_20260701_part1.txt.zst"}, true), "\n")
	if !strings.Contains(dry, "DRY RUN") {
		t.Fatalf("dry-run ingest summary missing DRY RUN title:\n%s", dry)
	}
}

func TestRenderFinalSummaryUpdateNoticeFooter(t *testing.T) {
	lines := renderFinalSummaryWithNotice("out/sfl.txt", sflog.ExtractStats{
		Emitted: 1,
	}, &selfupdate.Notice{Latest: "9.9.9", Command: "sfl update"}, nil)
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
		"Unique",
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

func TestRenderNoIngestSummaryOmitsLibraryUnchangedLine(t *testing.T) {
	lines := renderNoIngestSummary("/data/Library", sflog.ExtractStats{
		Logs:             1,
		ArchivesScanned:  1,
		SkippedArchives:  1,
		PasswordNotFound: 1,
		Issues:           []sflog.Issue{{Path: "/data/locked.zip", Kind: sflog.IssuePasswordNotFound}},
	}, false)
	joined := strings.Join(lines, "\n")
	// The "No credentials extracted — library unchanged." line was cut
	// (2026-10-03, user request): the box keeps its trailing blank rows.
	for _, absent := range []string{"library unchanged", "No credentials extracted"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("no-ingest summary must not contain %q:\n%s", absent, joined)
		}
	}
	// The Library path footer was cut (2026-10-01): the user configures the
	// library and doesn't need it echoed back.
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
	lines := renderProgress(0, prog, 0, 0, 80, true)
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
	joined := strings.Join(renderProgress(0, prog, 0, 0, 80, true), "\n")
	for _, want := range []string{"sfl is open-source", "snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frame missing footer %q:\n%s", want, joined)
		}
	}
}

// Frost styling must keep the ❤️ cluster intact; per-rune SGR used to split
// ❤ from VS16, under-count width by 1, and soft-wrap junk onto the next row.
func TestFrostTaglineHeartKeepsEmojiWidth(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	plain := sflFooterLine1
	styled := frostTagline(plain, 0.0, 0.5)
	if got, want := tuiVisibleWidth(styled), tuiVisibleWidth(plain); got != want {
		t.Fatalf("styled width %d != plain width %d (heart cluster split?)", got, want)
	}
}

// frostTaglineRight must clamp over-budget rows like sfu/sfs (no soft-wrap).
func TestFrostTaglineRightHonorsBudget(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	for _, width := range []int{1, 2, 4, 10} {
		got := frostTaglineRight(sflFooterLine1, width, 0.0, 0.5)
		if w := tuiVisibleWidth(got); w > width {
			t.Fatalf("width=%d: row width %d exceeds budget: %q", width, w, got)
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
// credential actions name their target, and every label fits the fixed
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
	joined := strings.Join(renderExtractStatsRows(files, archives, 0, logs, logsTot, emitted, dupes, done, total, rate), "\n")
	for _, want := range []string{
		"12,345,678",
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
	rows := renderExtractStatsRows(10, 2, 0, 3, 5, 7, 1, 1<<20, 10<<20, 5<<20)
	for _, label := range []string{"Unique", "Sources", "Bytes"} {
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
	if len(rows) != 3 {
		t.Fatalf("want 3 stats rows (Input counter removed, ETA removed), got %d", len(rows))
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

// TestSflPlainExtractFrameFitsTerminal guards against footer flicker: the
// single-bar plain frame height is exactly
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
// sfu-style stacked layout: a stats box, both stage bars (live Extract +
// queued Deduping) on their own lines below it, and the worker panel in its
// own box. It builds the frame from the same helpers the plain branch of
// renderProgress composes, so a wiring regression here is caught without
// needing to run the engine to set Progress.Total.
func TestSflPlainExtractFrameStacked(t *testing.T) {
	prog := sflog.NewProgress()
	prog.SetWorkers(4)
	const width = 80
	inner := boxInner(width)
	header := headerLine(sflSpinnerStyle.Render("·"), sflOkStyle.Render("[sfl] EXTRACTING"), 0, width)
	statRows := renderExtractStatsRows(1, 1, 0, 1, 10, 5, 2, 1<<19, 1<<20, 1e6)
	plainBox := sflGradientBox(statRows, width, gradStart, gradEnd)
	plainBars := []string{
		sflIndent + sflBarLabel("Extract") + gradientBar(prog.Fraction(), sflBarBody(width)),
		sflIndent + sflBarLabel("Deduping") + sflPendingBar(sflBarBody(width)),
	}
	panel := sflWorkerPanelBox(prog, width, inner, 0, sflPlainFrameOverhead)
	lines := sflFrameWithBars(header, plainBox, plainBars, panel, width)
	joined := strings.Join(lines, "\n")

	if !strings.Contains(joined, sflBarLabel("Extract")) {
		t.Fatalf("plain frame missing the Extract bar:\n%s", joined)
	}
	// Both stage bars render during extraction, mirroring sfu: live Extract
	// plus a queued Deduping track (no fake 0.0%).
	if !strings.Contains(joined, sflBarLabel("Deduping")+sflPendingBar(sflBarBody(width))) {
		t.Fatalf("plain frame missing the queued Deduping bar:\n%s", joined)
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
		FilesScanned:     5,
		SkippedArchives:  2,
		SkippedFiles:     1,
		FailedSources:    3,
		PasswordNotFound: 2,
		Issues: []sflog.Issue{
			{Path: "/data/locked.zip", Kind: sflog.IssuePasswordNotFound},
		},
	})
	joined := strings.Join(lines, "\n")
	// Failed sources are deliberately not a recap row; the issues log and
	// exit code carry them (2026-09-30, user).
	if strings.Contains(joined, "Unopened") {
		t.Fatalf("summary must not show an Unopened row:\n%s", joined)
	}
	if strings.Contains(joined, "3 skipped") {
		t.Fatalf("summary must not show a redundant skip tail:\n%s", joined)
	}
	// ...but the encrypted/password signal lives ONLY in the automatic temp
	// issue log (see issue_test.go), so the summary box itself must not
	// duplicate it as an in-box row.
	if strings.Contains(joined, "no password matched") {
		t.Fatalf("summary box must not duplicate the encrypted signal:\n%s", joined)
	}
	// ...and per-issue detail is streamed to the automatic temp issue log,
	// never stdout.
	for _, absent := range []string{"passwords not found", "locked.zip"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("issue detail %q must not appear on stdout:\n%s", absent, joined)
		}
	}
}

// TestIssueFooterSplicedIntoSummary proves the summary-level placement of the
// automatic issue log footer (the replacement for the deleted red encrypted
// warning box): a muted path footer pointing at the temp TSV, spliced before
// the frost footer like the Env footer.
func TestIssueFooterSplicedIntoSummary(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	issuePath := "/tmp/sfl-issues-20260920_test13-123456.log"
	res := issueLogResult{Path: issuePath, Count: 2}
	summary := renderFinalSummaryWithNotice("out/sfl.txt", sflog.ExtractStats{Emitted: 1}, nil, nil)
	frost := summaryFooterLines(termWidth(), nil)
	if block := issueFooterBlock(res); block != nil {
		summary = spliceBeforeFooter(summary, block, frost)
	}

	joined := strings.Join(summary, "\n")
	if !strings.Contains(joined, issuePath) {
		t.Fatalf("issue log path must appear in full:\n%s", joined)
	}
	closeIdx := strings.LastIndex(joined, "╰")
	issueIdx := strings.Index(joined, issuePath)
	if closeIdx < 0 || issueIdx < closeIdx {
		t.Fatalf("issue path should be below the COMPLETE frame:\n%s", joined)
	}
	// No remediation prose and no red warning block may return.
	for _, absent := range []string{"ENCRYPTED", "No password was supplied", "sfl -p"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("summary must not resurrect the encrypted warning prose %q:\n%s", absent, joined)
		}
	}
}

// A zero-issue or failed issue log must produce no Issues footer at all.
func TestIssueFooterAbsentWithoutIssues(t *testing.T) {
	summary := renderFinalSummaryWithNotice("out/sfl.txt", sflog.ExtractStats{Emitted: 1}, nil, nil)
	joined := strings.Join(summary, "\n")
	if strings.Contains(joined, "Issues") {
		t.Fatalf("no Issues footer when the log is empty:\n%s", joined)
	}
	if issueFooterBlock(issueLogResult{Count: 2, Err: errors.New("boom")}) != nil {
		t.Fatal("failed close must not produce an Issues footer")
	}
	if issueFailureLine(issueLogResult{Count: 0}) != "" {
		t.Fatal("clean close must produce no failure line")
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
const longUUIDLibraryPath = "/run/media/user/012e7890-aaaa-bbbb-cccc-dddddddddddd/Data_logs"

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

// The Library path footer was cut (2026-10-01, user request): the long
// library path must not reappear anywhere in the no-ingest summary. Long
// outside-box path rendering stays covered by the Output footer test.
func TestRenderNoIngestSummaryLongLibraryPathOutsideBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	lines := renderNoIngestSummary(longUUIDLibraryPath, sflog.ExtractStats{Logs: 1}, false)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, longUUIDLibraryPath) {
		t.Fatalf("library path should be cut from the summary:\n%s", joined)
	}
	if strings.Contains(joined, "Library:") {
		t.Fatalf("in-box 'Library:' label should be gone:\n%s", joined)
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
	summary := renderFinalSummaryWithNotice("out/sfl.txt", stats, nil, nil)
	frost := summaryFooterLines(termWidth(), nil)
	summary = spliceBeforeFooter(summary,
		renderSflPathFooter("Envs     ", []string{envDir}, sflMutedStyle), frost)

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
	if !strings.Contains(joined, "Env") || !strings.Contains(joined, "copied") {
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
	summary := renderFinalSummaryWithNotice("out/sfl.txt", stats, nil, nil)
	frost := summaryFooterLines(termWidth(), nil)
	summary = spliceBeforeFooter(summary,
		renderSflPathFooter("Envs     ", []string{envDir}, sflMutedStyle), frost)

	joined := strings.Join(summary, "\n")
	if !strings.Contains(joined, envDir) {
		t.Fatalf("env path must appear when only tdata folders copied:\n%s", joined)
	}
	if !strings.Contains(joined, "tdata folder") {
		t.Fatalf("recap should show tdata tail on the Env row:\n%s", joined)
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
	lines := renderInterrupt(0, "/", nil, 80, nil)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"INTERRUPTED", "force-exit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("interrupt frame missing %q:\n%s", want, joined)
		}
	}
}

func TestRenderInterruptShowsCleanupLog(t *testing.T) {
	log := []string{"removed temp dir /tmp/sfl-spill-abc", "removed /out/sfl_partial.txt"}
	joined := strings.Join(renderInterrupt(0, "/", nil, 80, log), "\n")
	for _, want := range log {
		if !strings.Contains(joined, want) {
			t.Fatalf("interrupt frame missing cleanup line %q:\n%s", want, joined)
		}
	}
}

// TestRenderInterruptShowsCleanupCount covers the nil-prog branch of
// liveInterruptStatus: when no Progress is wired in (early shutdown / tests)
// but cleanup has started, the live line reports the count of removed temp
// files instead of the static "Waiting for workers" fallback.
//
// The busy-worker branch of liveInterruptStatus isn't
// unit-tested here because *sflog.Progress exposes no exported way to mark a
// worker slot active; it's covered indirectly by
// the engine integration run. The sfu sibling test (TestRenderInterruptLinesShowsDrainCount)
// covers the parallel drain-count rendering on ulpengine.Metrics, whose
// fields are exported atomics.
func TestRenderInterruptShowsCleanupCount(t *testing.T) {
	log := []string{
		"removed temp dir /tmp/sfl-spill-abc",
		"removed /out/sfl_partial.txt",
		"removed /out/sfl_partial-2.txt",
	}
	joined := strings.Join(renderInterrupt(0, "/", nil, 80, log), "\n")
	if !strings.Contains(joined, "Removing temp files — 3 removed.") {
		t.Fatalf("interrupt frame missing live cleanup count:\n%s", joined)
	}
}

// -odr dry-run: ingest summary title becomes "SnowFastLog DRY RUN", the
// Added row is relabeled "Unique", and the output footer states nothing
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
		"Unique",
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
	joined := strings.Join(renderProgress(0, prog, 0, 0, 80, true), "\n")
	if !strings.Contains(joined, "DRY RUN") {
		t.Fatalf("dry-run live header missing DRY RUN marker:\n%s", joined)
	}
}

// -od ingest relocates the library total into the Library stat row
// ("· N lines"); the old "vs library" header narration and the muted
// bottom status line are gone, while iv.Status stays in the model.
func TestRenderProgressLibraryCountRow(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	plain := strings.Join(renderProgress(0, sflog.NewProgress(), 0, 0, 80, true), "\n")
	if strings.Contains(plain, "vs library") {
		t.Fatalf("non-od run must not show a library badge:\n%s", plain)
	}

	prog := sflog.NewProgress()
	prog.SetLibrary(true)
	extract := strings.Join(renderProgress(0, prog, 0, 0, 80, true), "\n")
	if strings.Contains(extract, "vs library") {
		t.Fatalf("od extract header must not narrate the library:\n%s", extract)
	}

	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			ODPhase:         int32(ulpengine.ODPhaseRegen),
			RegenBytesTotal: 1 << 20,
			RegenBytesRead:  1 << 19,
			LibraryKeys:     3_290_076_168,
			Status:          "merging…",
		}
	})
	ingest := strings.Join(renderProgress(0, prog, 0, 0, 80, true), "\n")
	if !strings.Contains(ingest, "[1/2 LIBRARY PREP]") {
		t.Fatalf("ingest prep header missing [1/2 LIBRARY PREP]:\n%s", ingest)
	}
	if !strings.Contains(ingest, "3.29B lines") {
		t.Fatalf("ingest Library row missing relocated count:\n%s", ingest)
	}
	if strings.Contains(ingest, "vs 3.29B library") || strings.Contains(ingest, "vs library") {
		t.Fatalf("ingest frame must not narrate vs library:\n%s", ingest)
	}
	// narration statuses stay in the progress model but never render
	if strings.Contains(ingest, "merging…") {
		t.Fatalf("ingest frame must not render the muted status line:\n%s", ingest)
	}
}

// the one-time library upgrade keeps the sfu-parity warn badge on the OD
// box's Library title row; the "upgrading library — do not interrupt" prose
// line is gone with the other narration.
func TestRenderProgressUpgradeWarnBadge(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	prog := sflog.NewProgress()
	prog.BeginIngest(func() sflog.IngestView {
		return sflog.IngestView{
			ODPhase:         int32(ulpengine.ODPhaseUpgrade),
			PartsRegenTotal: 56,
			PartsRegenDone:  39,
			Status:          "upgrading library — do not interrupt",
		}
	})
	ingest := strings.Join(renderProgress(0, prog, 0, 0, 80, true), "\n")
	if !strings.Contains(ingest, "DO NOT INTERRUPT") {
		t.Fatalf("upgrade ingest frame missing DO NOT INTERRUPT badge:\n%s", ingest)
	}
	if strings.Contains(ingest, "upgrading library") {
		t.Fatalf("upgrade ingest frame must not narrate the phase:\n%s", ingest)
	}
}

// The Sources row must read as English for singular counts ("1 file"),
// not "1 files".
func TestRenderExtractStatsRowsSingularCounts(t *testing.T) {
	joined := stripANSI(strings.Join(renderExtractStatsRows(1, 1, 0, 1, 10, 5, 2, 1<<19, 1<<20, 1e6), "\n"))
	if !strings.Contains(joined, "1 file") {
		t.Fatalf("singular Sources row missing %q:\n%s", "1 file", joined)
	}
	if strings.Contains(joined, "1 files") {
		t.Fatalf("singular Sources row still renders %q:\n%s", "1 files", joined)
	}
	if strings.Contains(joined, "log") {
		t.Fatalf("Sources row must not show the logs counter:\n%s", joined)
	}
}

// Finished Extract bar uses dusty mauve (#8A5A68), not sfu's sage green.
func TestSflDoneFillIsDustyMauve(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	out := sflSolidBar(1.0, 40, sflDoneFill)
	// lipgloss/#8A5A68 emits G=89 (not 90) under TrueColor.
	if !strings.Contains(out, "38;2;138;89;104") {
		t.Fatalf("done fill want dusty mauve #8A5A68 SGR, got %q", out)
	}
}

// Final summary adds a gold Output size row (sfu parity) when the path exists.
func TestRenderFinalSummaryOutputSizeRow(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	outPath := filepath.Join(t.TempDir(), "sfl.txt")
	if err := os.WriteFile(outPath, []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(renderFinalSummary(outPath, sflog.ExtractStats{
		ArchivesScanned: 1, Credentials: 10, Emitted: 8, Duplicates: 2,
	}), "\n")
	plain := stripANSI(joined)
	if !strings.Contains(plain, "Output") || !strings.Contains(plain, "2.0 KB") {
		t.Fatalf("summary missing gold Output size row:\n%s", plain)
	}
	if !strings.Contains(joined, sflByteStyle.Render(humanBytes(2048))) {
		t.Fatalf("Output size must use byteStyle (gold):\n%s", joined)
	}
	if !strings.Contains(joined, sflUniqueStyle.Render(formatInt(8))) {
		t.Fatalf("Unique must use uniqueStyle (green):\n%s", joined)
	}
	if !strings.Contains(joined, sflCountStyle.Render(formatInt(10))) {
		t.Fatalf("Lines count must use countStyle (cyan):\n%s", joined)
	}
}

// Missing output path must not invent an Output size row inside the box.
func TestRenderFinalSummaryOmitsOutputSizeWhenMissing(t *testing.T) {
	joined := stripANSI(strings.Join(renderFinalSummary("out/does-not-exist.txt", sflog.ExtractStats{
		Credentials: 1, Emitted: 1,
	}), "\n"))
	// Path footer still labels Output; the in-box size row would sit between
	// Lines and Unique. Require Unique to follow Lines with no KB/MB/GB size.
	linesIdx := strings.Index(joined, "Lines")
	uniqIdx := strings.Index(joined, "Unique")
	if linesIdx < 0 || uniqIdx <= linesIdx {
		t.Fatalf("expected Lines then Unique:\n%s", joined)
	}
	between := joined[linesIdx:uniqIdx]
	for _, unit := range []string{" KB", " MB", " GB", " B\n", " B "} {
		if strings.Contains(between, unit) {
			t.Fatalf("missing path must not show Output size between Lines/Unique (%q):\n%s", unit, joined)
		}
	}
}

// sflDoneHeader pins the final frame's harmonized header: sfu's COMPLETE
// shape (✓ + phase left, muted clock flush right), DRY RUN variant included.
func TestSflDoneHeaderShape(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	const width = 86
	line := sflDoneHeader(false, 95*time.Second, width)
	plain := stripANSI(line)
	if !strings.HasPrefix(plain, sflIndent+"✓  COMPLETE") {
		t.Fatalf("header must start with indent + ✓ + COMPLETE; got %q", plain)
	}
	if !strings.HasSuffix(plain, "1m35s") {
		t.Fatalf("header must end with the formatted duration; got %q", plain)
	}
	if w := lipgloss.Width(plain); w != width-sflLeftPad {
		t.Fatalf("header width %d, want %d (width - gutter)", w, width-sflLeftPad)
	}
	if plain := stripANSI(sflDoneHeader(true, time.Second, width)); !strings.Contains(plain, "COMPLETE · DRY RUN") {
		t.Fatalf("dry-run header must carry the DRY RUN suffix; got %q", plain)
	}
}
