package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                       "00:00:00",
		1500 * time.Millisecond: "00:00:01",
		61 * time.Second:        "00:01:01",
		3661 * time.Second:      "01:01:01",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFormatCountThousands(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		999:       "999",
		1000:      "1,000",
		1_234_567: "1,234,567",
		-1234:     "-1,234",
	}
	for n, want := range cases {
		if got := formatCount(n); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	if humanBytes(-1) != "0 B" {
		t.Errorf("negative should clamp to 0 B")
	}
	if humanBytes(512) != "512 B" {
		t.Errorf("under 1k stays bytes")
	}
	if got := humanBytes(1536); got != "1.5 KB" {
		t.Errorf("1536 -> %q", got)
	}
	if got := humanBytes(1 << 30); got != "1.0 GB" {
		t.Errorf("1 GiB -> %q", got)
	}

	// [1000, 1024) must roll up so display stays <=8 chars. pre-fix:
	// "1000.0 MB" / "1023.0 KB" drifted the bar col
	if got := humanBytes(1000 * 1024 * 1024); got != "1.0 GB" {
		t.Errorf("1000 MiB should roll up to GB; got %q", got)
	}
	if got := humanBytes(1023 * 1024); got != "1.0 MB" {
		t.Errorf("1023 KiB should roll up to MB; got %q", got)
	}
	// 999 stays put, threshold is >= 1000
	if got := humanBytes(999 * 1024 * 1024); got != "999.0 MB" {
		t.Errorf("999 MiB should remain in MB; got %q", got)
	}
}

// 8-char slot promise for worker-row / OD-frame byte columns
func TestHumanBytesNeverExceedsEightVisibleChars(t *testing.T) {
	cases := []int64{
		0, 1, 999, 1000, 1023, 1024,
		999 * 1024, 1000 * 1024, 1023 * 1024,
		999 * 1024 * 1024, 1000 * 1024 * 1024, 1023 * 1024 * 1024,
		1<<30 - 1, 1 << 30, 1<<30 + 1,
		999 * (1 << 30), 1023 * (1 << 30),
		1 << 40, 1 << 50,
	}
	for _, n := range cases {
		got := humanBytes(n)
		if len(got) > 8 {
			t.Errorf("humanBytes(%d) = %q (%d chars); slot reserves 8", n, got, len(got))
		}
	}
}

// -od runs expose 3 labeled steps, plain runs use 2
func TestRenderPhaseTagCounts(t *testing.T) {
	now := time.Now()
	m := &ulpengine.Metrics{}
	base := &ulpengine.Resolved{TotalInputs: 1, Workers: 1, DedupWorkers: 1, BucketCount: 1}

	noOD := strings.Join(renderShardLines(now, time.Second, m, base, 0, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(noOD, "[1/2 PARSING]") {
		t.Errorf("non-od shard: want [1/2 PARSING]\n%s", noOD)
	}
	noODDedup := strings.Join(renderDedupLines(now, time.Second, m, base, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(noODDedup, "[2/2 DEDUPING]") {
		t.Errorf("non-od dedup: want [2/2 DEDUPING]\n%s", noODDedup)
	}

	withOD := *base
	withOD.Cfg.DestDedup = true
	withOD.OdMetrics = &ulpengine.ODMetrics{}
	withOD.OdMetrics.Phase.Store(int32(ulpengine.ODPhaseRegen))

	p0 := strings.Join(renderPhase0Lines(time.Second, m, &withOD, 0, 0, 0, 86), "\n")
	if !strings.Contains(p0, "[1/2 LIBRARY PREP]") {
		t.Errorf("od phase0: want [1/2 LIBRARY PREP]\n%s", p0)
	}
	if strings.Contains(p0, "INDEXING LIBRARY") {
		t.Errorf("od phase0 should not have separate library header\n%s", p0)
	}
	p1 := strings.Join(renderShardLines(now, time.Second, m, &withOD, 0, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(p1, "[1/2 PARSING]") {
		t.Errorf("od shard: want [1/2 PARSING]\n%s", p1)
	}
	p2 := strings.Join(renderDedupLines(now, time.Second, m, &withOD, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(p2, "[2/2 DEDUPING]") {
		t.Errorf("od dedup: want [2/2 DEDUPING]\n%s", p2)
	}
}

func TestRenderStep1PhaseTagSwitchesAfterInputsRead(t *testing.T) {
	r := &ulpengine.Resolved{TotalInputs: 1000, Workers: 1, DedupWorkers: 1, BucketCount: 1}
	r.Cfg.DestDedup = true
	r.OdMetrics = &ulpengine.ODMetrics{}
	r.OdMetrics.Phase.Store(int32(ulpengine.ODPhaseUpgrade))

	m := &ulpengine.Metrics{}
	if got := renderStep1PhaseTag(r, m); !strings.Contains(got, "PARSING") {
		t.Errorf("while inputs still reading: want PARSING tag, got %q", got)
	}

	m.ChunksTotal.Store(10)
	m.ChunksDone.Store(10)
	if got := renderStep1PhaseTag(r, m); !strings.Contains(got, "LIBRARY PREP") {
		t.Errorf("after inputs read w/ od in flight: want LIBRARY PREP tag, got %q", got)
	}

	r.OdMetrics.Phase.Store(int32(ulpengine.ODPhaseDone))
	if got := renderStep1PhaseTag(r, m); !strings.Contains(got, "PARSING") {
		t.Errorf("after od done: revert to PARSING tag, got %q", got)
	}
}

func TestRenderShardLinesFitsWidth(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.BytesRead.Store(1 << 30)
	m.ChunksDone.Store(42)
	m.ChunksTotal.Store(205)
	m.LinesRead.Store(78_499_000)
	m.LinesAccepted.Store(78_304_811)
	m.LinesRejected.Store(194_189)
	m.BusyWorkers.Store(8)
	r := &ulpengine.Resolved{
		TotalInputs:    18 << 30,
		InputFileCount: 4,
		Workers:        8,
		DedupWorkers:   4,
		BucketCount:    256,
	}
	for _, w := range []int{80, 60} {
		now := time.Date(2026, 5, 9, 22, 30, 0, 0, time.UTC)
		lines := renderShardLines(now, 90*time.Second, m, r, 320.5, 540.0, 320e6, 280e6, 0, w)
		if len(lines) < 11 {
			t.Fatalf("width=%d: want >=11 lines, got %d", w, len(lines))
		}
		for i, ln := range lines {
			if vw := tuiVisibleWidth(ln); vw > w {
				t.Errorf("width=%d line %d visible width %d > %d: %q", w, i, vw, w, ln)
			}
		}
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "[1/2 PARSING]") {
			t.Errorf("width=%d: missing phase tag in: %q", w, joined)
		}
		for _, want := range []string{"sfu is open-source", "https://snowx.dev"} {
			if !strings.Contains(joined, want) {
				t.Errorf("width=%d: missing footer %q in: %q", w, want, joined)
			}
		}
	}
}

// first line w/ every needle, survives layout reshuffles
func findRow(lines []string, needles ...string) string {
	for _, ln := range lines {
		ok := true
		for _, n := range needles {
			if !strings.Contains(ln, n) {
				ok = false
				break
			}
		}
		if ok {
			return ln
		}
	}
	return ""
}

// adding a digit to read rate must not shift the "shard" label column
func TestRenderShardThroughputColumnsAreStable(t *testing.T) {
	m := &ulpengine.Metrics{}
	r := &ulpengine.Resolved{TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4, DedupWorkers: 2, BucketCount: 64}

	low := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 93e6, 111e6, 0, 80)
	high := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 999e6, 1234e6, 0, 80)

	lowRow := findRow(low, "Throughput", "shard")
	highRow := findRow(high, "Throughput", "shard")
	if lowRow == "" || highRow == "" {
		t.Fatalf("throughput row not found: low=%q high=%q", lowRow, highRow)
	}
	idxLow := strings.Index(lowRow, "shard")
	idxHigh := strings.Index(highRow, "shard")
	if idxLow != idxHigh {
		t.Errorf("shard column shifted: low=%d high=%d\n  low : %q\n  high: %q",
			idxLow, idxHigh, lowRow, highRow)
	}
}

// bytes-read, chunks-done, workers cols stay put as values grow
func TestRenderShardProgressColumnsAreStable(t *testing.T) {
	r := &ulpengine.Resolved{TotalInputs: 18 << 30, InputFileCount: 1, Workers: 8, DedupWorkers: 4, BucketCount: 256}

	mEarly := &ulpengine.Metrics{}
	mEarly.BytesRead.Store(4 * 1 << 30) // 4 GB
	mEarly.ChunksDone.Store(46)
	mEarly.ChunksTotal.Store(205)
	mEarly.BusyWorkers.Store(2)

	mLate := &ulpengine.Metrics{}
	mLate.BytesRead.Store(15 * 1 << 30) // 15 GB, diff digit count
	mLate.ChunksDone.Store(199)
	mLate.ChunksTotal.Store(205)
	mLate.BusyWorkers.Store(8)

	early := renderShardLines(time.Now(), time.Second, mEarly, r, 100, 100, 1e6, 1e6, 0, 80)
	late := renderShardLines(time.Now(), time.Second, mLate, r, 100, 100, 1e6, 1e6, 0, 80)

	earlyRow := findRow(early, "Progress", "chunks")
	lateRow := findRow(late, "Progress", "chunks")
	if earlyRow == "" || lateRow == "" {
		t.Fatalf("progress row not found: early=%q late=%q", earlyRow, lateRow)
	}
	for _, anchor := range []string{"chunks", "workers"} {
		if a, b := strings.Index(earlyRow, anchor), strings.Index(lateRow, anchor); a != b {
			t.Errorf("%q column shifted: early=%d late=%d\n  early: %q\n  late : %q",
				anchor, a, b, earlyRow, lateRow)
		}
	}
}

func TestRenderShardLinesShowsByteWeightedChunkProgress(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.BytesRead.Store(50)
	m.ChunksDone.Store(0)
	m.ChunksTotal.Store(16)
	r := &ulpengine.Resolved{TotalInputs: 100, InputFileCount: 1, Workers: 8, DedupWorkers: 4, BucketCount: 64}

	lines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	row := findRow(lines, "Progress", "chunks")
	if row == "" {
		t.Fatalf("progress row not found in:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(row, "chunks  8.0 / 16") {
		t.Fatalf("progress row = %q, want byte-weighted chunk progress", row)
	}
}

func TestRenderShardLinesShowsFastPathWorkerBusy(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.BytesRead.Store(50)
	m.ChunksDone.Store(0)
	m.ChunksTotal.Store(1)
	r := &ulpengine.Resolved{
		TotalInputs:    100,
		InputFileCount: 1,
		UseFastPath:    true,
		Workers:        8,
		DedupWorkers:   4,
		BucketCount:    64,
	}

	lines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	row := findRow(lines, "Progress", "workers")
	if row == "" {
		t.Fatalf("progress row not found in:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(row, "1 / 1 busy") {
		t.Fatalf("fast-path progress row = %q, want single busy worker", row)
	}
	if strings.Contains(row, "0 / 8 busy") {
		t.Fatalf("fast-path progress row exposed bucket worker pool: %q", row)
	}
}

func TestRenderShardLinesMirrorsDedupBarForFastPath(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.BytesRead.Store(50)
	m.ChunksDone.Store(0)
	m.ChunksTotal.Store(1)
	r := &ulpengine.Resolved{
		TotalInputs:    100,
		InputFileCount: 1,
		UseFastPath:    true,
		Workers:        8,
		DedupWorkers:   4,
		BucketCount:    64,
	}

	lines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	parsing := findRow(lines, "Parsing", "50.0%")
	deduping := findRow(lines, "Deduping", "50.0%")
	if parsing == "" {
		t.Fatalf("fast-path parsing bar did not show 50%% progress in:\n%s", strings.Join(lines, "\n"))
	}
	if deduping == "" {
		t.Fatalf("fast-path deduping bar did not mirror parsing progress in:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(deduping, "----") {
		t.Fatalf("fast-path deduping bar should not be pending: %q", deduping)
	}
}

func TestRenderShardLinesKeepsDedupBarPendingForBucketedParsing(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.BytesRead.Store(50)
	m.ChunksDone.Store(0)
	m.ChunksTotal.Store(16)
	r := &ulpengine.Resolved{
		TotalInputs:    100,
		InputFileCount: 1,
		Workers:        8,
		DedupWorkers:   4,
		BucketCount:    64,
	}

	lines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	deduping := findRow(lines, "Deduping", "----")
	if deduping == "" {
		t.Fatalf("bucketed parsing should keep deduping pending in:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(deduping, "50.0%") {
		t.Fatalf("bucketed parsing deduping bar should not mirror parse progress: %q", deduping)
	}
}

// bars start at col 4, aligned w/ stat rows above
func TestRenderShardBarsAreIndented(t *testing.T) {
	m := &ulpengine.Metrics{}
	r := &ulpengine.Resolved{TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4, DedupWorkers: 2, BucketCount: 64}
	lines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	// bars above frost footer, identify by indent + glyphs
	var bar1, bar2 string
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "    ") {
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
	if !strings.HasPrefix(bar1, "    ") {
		t.Errorf("bar1 should start with 4-space indent, got: %q", bar1[:8])
	}
	if !strings.HasPrefix(bar2, "    ") {
		t.Errorf("bar2 should start with 4-space indent, got: %q", bar2[:8])
	}
	// balanced layout: leftPad (4) indent + content (width-2*leftPad=72) leaves
	// a matching 4-col right margin, so the bar spans 76 of the 80 cols.
	if w := tuiVisibleWidth(bar1); w != 76 {
		t.Errorf("bar1 visible width = %d, want 76", w)
	}
	// right edge must sit one leftPad in from the terminal edge (balanced).
	if w := tuiVisibleWidth(bar1); w > 80-leftPad {
		t.Errorf("bar1 right edge not balanced: width %d exceeds %d", w, 80-leftPad)
	}
}

func TestRenderMainProgressBarsShowPhaseLabels(t *testing.T) {
	m := &ulpengine.Metrics{}
	r := &ulpengine.Resolved{TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4, DedupWorkers: 2, BucketCount: 64}
	m.BytesRead.Store(1 << 29)

	parseLines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	parseJoined := strings.Join(parseLines, "\n")
	for _, want := range []string{"Parsing", "Deduping", "----"} {
		if !strings.Contains(parseJoined, want) {
			t.Fatalf("parsing phase missing %q in:\n%s", want, parseJoined)
		}
	}

	m.BucketsTotal.Store(8)
	m.BucketsBytesTotal.Store(1000)
	m.BucketsBytesRead.Store(500)
	dedupLines := renderDedupLines(time.Now(), time.Second, m, r, 100, 100, 1e6, 0, 80)
	dedupJoined := strings.Join(dedupLines, "\n")
	for _, want := range []string{"Parsing", "Deduping", "█", "░", "%"} {
		if !strings.Contains(dedupJoined, want) {
			t.Fatalf("dedup phase missing %q in:\n%s", want, dedupJoined)
		}
	}
}

// rejected count rendered muted, exact-match against mutedStyle.Render
func TestRejectedIsMuted(t *testing.T) {
	got := renderRejected(4_675_099)
	want := mutedStyle.Render("4,675,099 rejected")
	if got != want {
		t.Errorf("renderRejected = %q, want %q (mutedStyle wrap)", got, want)
	}
}

// counts >= 1000 always use thousands separators, no K/M/B shorthand
func TestRenderShardUsesCommasInLineCounts(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesRead.Store(78_499_000)
	m.LinesAccepted.Store(78_304_811)
	m.LinesRejected.Store(194_189)
	m.BytesRead.Store(1 << 30)
	r := &ulpengine.Resolved{TotalInputs: 18 << 30, InputFileCount: 1, Workers: 8, DedupWorkers: 4, BucketCount: 256}
	now := time.Now()
	lines := renderShardLines(now, time.Second, m, r, 320, 540, 1, 1, 0, 80)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"78,499,000", "78,304,811", "194,189"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing comma-formatted count %q in:\n%s", want, joined)
		}
	}
	for _, banned := range []string{"78.5M", "78M ", "194K", "78.3M"} {
		if strings.Contains(joined, banned) {
			t.Errorf("unexpected shorthand %q present:\n%s", banned, joined)
		}
	}
}

func TestRenderDedupAndDoneFitsWidth(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesUnique.Store(80_000_000)
	m.LinesAccepted.Store(80_234_000)
	m.BucketsDone.Store(256)
	m.BucketsTotal.Store(256)
	m.BytesWritten.Store(7 << 30)
	m.LinesRejected.Store(14_000_000)
	// short relative path keeps 80/60-col asserts honest, t.TempDir
	// would bust the budget
	r := &ulpengine.Resolved{
		Cfg:            ulpengine.Config{Output: "out.txt"},
		TotalInputs:    18 << 30,
		InputFileCount: 3,
		Workers:        8,
		DedupWorkers:   4,
		BucketCount:    256,
	}
	for _, w := range []int{80, 60} {
		now := time.Now()
		ded := renderDedupLines(now, 48*time.Second, m, r, 290.1, 410.0, 240e6, 0, w)
		done := renderFinalStdoutSummary(131*time.Second, m, r, w, nil)
		for _, ln := range append(ded, done...) {
			if vw := tuiVisibleWidth(ln); vw > w {
				t.Errorf("width=%d line visible width %d > %d: %q", w, vw, w, ln)
			}
		}
	}
}

// DONE block surfaces every metric user expects
func TestRenderDoneIncludesAllSummaryFields(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesUnique.Store(77_500_000)
	m.LinesAccepted.Store(77_734_000) // unique + 234,000 dups
	m.LinesRejected.Store(166_172)
	m.BytesWritten.Store(8_800_000_000) // ~8.2 GB
	r := &ulpengine.Resolved{
		Cfg:            ulpengine.Config{Output: "./sfu_20260509_abc123.txt"},
		OutputPaths:    []string{"./sfu_20260509_abc123.txt"}, // a real run that wrote lines sets this
		TotalInputs:    10_737_418_240,                        // 10 GB
		InputFileCount: 4,
	}
	lines := renderFinalStdoutSummary(102*time.Second, m, r, 80, nil)
	joined := strings.Join(lines, "\n")
	want := []string{
		"COMPLETE",
		"00:01:42",
		"10.0 GB",
		"4",
		"./sfu_",
		"77,500,000",
		"166,172",
		"234,000", // accepted - unique
	}
	for _, s := range want {
		if !strings.Contains(joined, s) {
			t.Errorf("DONE block missing %q in:\n%s", s, joined)
		}
	}
	for _, s := range []string{"sfu is open-source", "https://snowx.dev"} {
		if !strings.Contains(joined, s) {
			t.Errorf("final summary missing footer %q in:\n%s", s, joined)
		}
	}
}

func TestRenderFinalStdoutSummaryUpdateNoticeFooter(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesUnique.Store(1)
	r := &ulpengine.Resolved{Cfg: ulpengine.Config{Output: "./out.txt"}}
	notice := &selfupdate.Notice{Latest: "0.2.0", Command: "sfu update"}
	joined := strings.Join(renderFinalStdoutSummary(time.Second, m, r, 86, notice), "\n")
	for _, want := range []string{"Update available", "v0.2.0", "sfu update", "sfu is open-source", "https://snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in summary footer:\n%s", want, joined)
		}
	}
}

func TestRenderFinalStdoutSummaryNoNoticeUsesPlainFooter(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesUnique.Store(1)
	r := &ulpengine.Resolved{Cfg: ulpengine.Config{Output: "./out.txt"}}
	joined := strings.Join(renderFinalStdoutSummary(time.Second, m, r, 86, nil), "\n")
	if strings.Contains(joined, "Update available") {
		t.Fatalf("nil notice should not show update line:\n%s", joined)
	}
	for _, want := range []string{"sfu is open-source", "https://snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in footer:\n%s", want, joined)
		}
	}
}

func TestDefaultOutputNameFormat(t *testing.T) {
	stamp := ulpengine.RunStamp(time.Date(2026, 5, 9, 20, 35, 30, 0, time.UTC), "abc123")
	got := defaultOutputName(stamp)
	want := "sfu_20260509_abc123.txt"
	if got != want {
		t.Errorf("defaultOutputName = %q, want %q", got, want)
	}
}

func TestResolveOutputDir(t *testing.T) {
	existingDir := t.TempDir()
	existingFile := filepath.Join(existingDir, "real.txt")
	if err := os.WriteFile(existingFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cases := []struct {
		name       string
		in         string
		wantDir    string
		wantMkdir  bool
		wantErrStr string
	}{
		{"empty is CWD", "", ".", true, ""},
		{"whitespace is CWD", "   ", ".", true, ""},
		{"trailing slash ok", "./out/", "./out/", true, ""},
		{"existing dir ok", existingDir, existingDir, true, ""},
		{"plain file path rejected", "cleaned.txt", "", false, "must be a directory"},
		{"existing file rejected", existingFile, "", false, "must be a directory"},
		{"ambiguous path rejected", "./nope/out.txt", "", false, "must be a directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotDir, gotMkdir, err := resolveOutputDir("-o", c.in)
			if c.wantErrStr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrStr) {
					t.Fatalf("err = %v, want substring %q", err, c.wantErrStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if gotMkdir != c.wantMkdir {
				t.Errorf("autoMkdir = %v, want %v", gotMkdir, c.wantMkdir)
			}
			if c.wantDir == "." {
				if gotDir != "." {
					t.Errorf("dir = %q, want %q", gotDir, ".")
				}
				return
			}
			wantAbs, werr := filepath.Abs(c.wantDir)
			if werr != nil {
				t.Fatal(werr)
			}
			gotAbs, gerr := filepath.Abs(gotDir)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if gotAbs != wantAbs {
				t.Errorf("dir = %q (abs %q), want %q (abs %q)", gotDir, gotAbs, c.wantDir, wantAbs)
			}
		})
	}
}

func TestWithZstExt(t *testing.T) {
	cases := []struct {
		in       string
		compress bool
		want     string
	}{
		{"out.txt", false, "out.txt"},
		{"out.txt", true, "out.txt.zst"},
		{"out.txt.zst", true, "out.txt.zst"},
		{"OUT.TXT.ZST", true, "OUT.TXT.ZST"},
		{"/abs/path/out.txt", true, "/abs/path/out.txt.zst"},
		{"out.zst", true, "out.zst"},
	}
	for _, c := range cases {
		if got := ulpengine.WithZstExt(c.in, c.compress); got != c.want {
			t.Errorf("withZstExt(%q, %v) = %q, want %q", c.in, c.compress, got, c.want)
		}
	}
}

// dedup bar prefers byte-level metric over bucket-count ratio so it
// ticks immediately, not at first bucket completion
func TestRenderDedupBarUsesByteProgress(t *testing.T) {
	m := &ulpengine.Metrics{}
	// bucket-count says 0%, byte-count says ~50%
	m.BucketsTotal.Store(8)
	m.BucketsDone.Store(0)
	m.BucketsBytesTotal.Store(1000)
	m.BucketsBytesRead.Store(500)
	r := &ulpengine.Resolved{
		Cfg:          ulpengine.Config{Output: "out.txt"},
		Workers:      4,
		DedupWorkers: 2,
		BucketCount:  8,
	}
	lines := renderDedupLines(time.Now(), time.Second, m, r, 100, 100, 1e6, 0, 80)

	// dedup bar is the only partially-filled one
	var bar string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if len(t) == 0 {
			continue
		}
		if !strings.ContainsAny(t, "█░") {
			continue
		}
		nFilled := strings.Count(t, "█")
		nEmpty := strings.Count(t, "░")
		if nFilled > 0 && nEmpty > 0 {
			bar = t
		}
	}
	if bar == "" {
		t.Fatalf("did not find a partially-filled bar in:\n%s", strings.Join(lines, "\n"))
	}
	// pct2 ~= 0.5, between 25% and 75% filled. count-based fallback
	// would be 0 filled glyphs
	nFilled := strings.Count(bar, "█")
	nEmpty := strings.Count(bar, "░")
	if nFilled <= nEmpty/4 || nFilled >= nEmpty*4 {
		t.Errorf("expected bar around 50%% full, got %d filled / %d empty: %q", nFilled, nEmpty, bar)
	}
}

// inline header badges: compressing, and -od library scale
func TestRenderDedupHeaderShowsCompressingBadge(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.BucketsTotal.Store(256)
	m.BucketsDone.Store(64)
	for _, compress := range []bool{false, true} {
		r := &ulpengine.Resolved{
			Cfg:          ulpengine.Config{Output: "out.txt", Compress: compress},
			Workers:      8,
			DedupWorkers: 4,
			BucketCount:  256,
		}
		now := time.Date(2026, 5, 9, 22, 30, 0, 0, time.UTC)
		lines := renderDedupLines(now, 90*time.Second, m, r, 200, 100, 240e6, 0, 80)
		joined := strings.Join(lines, "\n")
		hasBadge := strings.Contains(joined, "compressing")
		if hasBadge != compress {
			t.Errorf("compress=%v: badge present=%v, want %v", compress, hasBadge, compress)
		}
		for _, ln := range lines {
			if w := tuiVisibleWidth(ln); w > 80 {
				t.Errorf("compress=%v: line width %d > 80: %q", compress, w, ln)
			}
		}
	}
}

func TestRenderDedupHeaderShowsLibraryBadge(t *testing.T) {
	m := &ulpengine.Metrics{}
	r := &ulpengine.Resolved{
		Cfg:          ulpengine.Config{Output: "out.txt.zst", Compress: true, DestDedup: true},
		Workers:      8,
		DedupWorkers: 4,
		BucketCount:  256,
	}
	r.OdMetrics = &ulpengine.ODMetrics{}
	r.OdMetrics.KeysTotalEstimate.Store(3_290_076_168)

	lines := renderDedupLines(time.Now(), time.Minute, m, r, 0, 0, 0, 0, 86)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"[2/2 DEDUPING]", "vs 3.29B library", "compressing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in dedup header/body\n%s", want, joined)
		}
	}
}

// DONE block reports on-disk size + (Nx compressed) when -zst is set
func TestRenderDoneShowsCompressionRatio(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "out.txt.zst")
	m := &ulpengine.Metrics{}
	// repeated line = dramatic ratio. write a real .zst directly so this TUI
	// test stays decoupled from the engine's (unexported) output sink; the
	// DONE ratio note is driven purely by m.BytesWritten vs on-disk size.
	const N = 200
	const line = "aaa.example.com:user@example.com:hunter2\n"
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		if _, err := enc.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	m.BytesWritten.Store(int64(N * len(line)))

	r := &ulpengine.Resolved{
		Cfg:            ulpengine.Config{Output: out, Compress: true},
		TotalInputs:    1 << 20,
		InputFileCount: 1,
	}
	lines := renderFinalStdoutSummary(60*time.Second, m, r, 80, nil)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "compressed") {
		t.Errorf("DONE block missing compression ratio note:\n%s", joined)
	}
	// compressed size string should appear, else stat err fell back
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined, humanBytes(fi.Size())) {
		t.Errorf("DONE block missing on-disk size %q:\n%s", humanBytes(fi.Size()), joined)
	}
}

func TestRenderDoneOutputFooterListsAllArchivesAligned(t *testing.T) {
	// short synthetic dir so 86-col budget holds. all parts share parent
	// so gutter collapses to one Output row + continuations
	const dir = "/x"
	r := &ulpengine.Resolved{
		Cfg: ulpengine.Config{Output: filepath.Join(dir, "sfu_20260509_abc123.txt.zst"), Compress: true},
		OutputPaths: []string{
			filepath.Join(dir, "sfu_20260509_abc123_part1.txt.zst"),
			filepath.Join(dir, "sfu_20260509_abc123_part2.txt.zst"),
			filepath.Join(dir, "sfu_20260509_abc123_part3.txt.zst"),
		},
	}
	lines := renderFinalStdoutSummary(time.Second, &ulpengine.Metrics{}, r, 80, nil)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "more parts") {
		t.Fatalf("should list paths explicitly, got:\n%s", joined)
	}
	for _, p := range r.OutputPaths {
		if !strings.Contains(joined, p) {
			t.Errorf("summary missing output path %q:\n%s", p, joined)
		}
	}
	// 2 output rows: 1 labeled "Output", rest are continuations
	var gutterRows int
	for _, ln := range lines {
		if strings.Contains(ln, "┃") && strings.Contains(ln, "Output") {
			gutterRows++
		}
	}
	if gutterRows != 1 {
		t.Errorf("want exactly one Output-labeled gutter row, got %d", gutterRows)
	}
	var continuation int
	for _, ln := range lines {
		if strings.Contains(ln, "┃") && strings.Contains(ln, "part2") && !strings.Contains(ln, "Output") {
			continuation++
		}
	}
	if continuation != 1 {
		t.Errorf("want one continuation row for part2, got %d", continuation)
	}
}

// TestRenderDoneOutputFooterNothingNew proves a run that wrote nothing (engine
// discarded the empty shard, so OutputPaths is empty) shows "(nothing new)"
// instead of resurrecting the removed Cfg.Output path via the live fallback.
func TestRenderDoneOutputFooterNothingNew(t *testing.T) {
	r := &ulpengine.Resolved{Cfg: ulpengine.Config{Output: "/lib/sfu_20260702_x.txt.zst"}}
	joined := strings.Join(renderDoneOutputFooter(r), "\n")
	if !strings.Contains(joined, "(nothing new)") {
		t.Fatalf("want (nothing new) footer, got:\n%s", joined)
	}
	if strings.Contains(joined, "sfu_20260702_x.txt.zst") {
		t.Fatalf("footer must not point at the removed Cfg.Output path:\n%s", joined)
	}
}

func TestRenderDoneOutputFooterPlainLongPath(t *testing.T) {
	// long suffix built inline, no checked-in personal mount path
	longPath := filepath.Join(os.TempDir(), strings.Repeat("nest/", 20)+"sfu_20260509-203530.txt")
	r := &ulpengine.Resolved{Cfg: ulpengine.Config{Output: longPath}, OutputPaths: []string{longPath}}
	lines := renderFinalStdoutSummary(time.Second, &ulpengine.Metrics{}, r, 80, nil)
	var pathLine string
	for _, ln := range lines {
		if strings.Contains(ln, longPath) {
			pathLine = ln
			break
		}
	}
	if pathLine == "" {
		t.Fatal("no summary line contains the output path")
	}
	if strings.ContainsRune(pathLine, '…') {
		t.Fatalf("path line should not be ellipsized: %q", pathLine)
	}
}

func TestTrimToDisplayWidthWithAnsi(t *testing.T) {
	// real lipgloss style so we test against the SGR sequences prod uses
	s := phaseStyle.Render("HELLOWORLD123")
	got := trimToDisplayWidth(s, 6)
	if w := tuiVisibleWidth(got); w > 6 {
		t.Fatalf("trim should respect width including ellipsis: w=%d, %q", w, got)
	}
}

// every spinner frame appears in one rotation, deterministic at instant
func TestSpinnerFrameCycles(t *testing.T) {
	base := time.Unix(0, 0)
	seen := map[string]bool{}
	// full cycle = len(frames) * 100ms, sample 2x to be safe
	for i := int64(0); i < int64(len(lineSpinnerFrames)*100*2); i += 100 {
		seen[spinnerFrame(base.Add(time.Duration(i)*time.Millisecond))] = true
	}
	if len(seen) != len(lineSpinnerFrames) {
		t.Errorf("expected %d distinct frames, got %d (%v)", len(lineSpinnerFrames), len(seen), seen)
	}
	t1 := spinnerFrame(time.Unix(123, 456_000_000))
	t2 := spinnerFrame(time.Unix(123, 456_000_000))
	if t1 != t2 {
		t.Errorf("spinnerFrame should be deterministic: %q vs %q", t1, t2)
	}
}

// both bar variants honor requested visible width across full percent range
func TestProgressBarRespectsWidth(t *testing.T) {
	cases := []struct {
		percent float64
		width   int
	}{
		{0.0, 60}, {0.5, 60}, {1.0, 60},
		{0.42, 80}, {0.999, 80},
	}
	for _, c := range cases {
		got := gradientBar(c.percent, c.width)
		if w := tuiVisibleWidth(got); w != c.width {
			t.Errorf("gradientBar(%.2f, %d) visible width = %d; want %d", c.percent, c.width, w, c.width)
		}
		got = solidBar(c.percent, c.width, solidGreenFill)
		if w := tuiVisibleWidth(got); w != c.width {
			t.Errorf("solidBar(%.2f, %d) visible width = %d; want %d", c.percent, c.width, w, c.width)
		}
	}
}

func TestFormatCompactCount(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{999_999, "999,999"},
		{1_000_000, "1.00M"},
		{3_290_076_168, "3.29B"},
	}
	for _, c := range cases {
		if got := formatCompactCount(c.n); got != c.want {
			t.Errorf("formatCompactCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestPendingBarRespectsWidth(t *testing.T) {
	for _, w := range []int{40, 60, 80} {
		got := pendingBar(w)
		if vw := tuiVisibleWidth(got); vw != w {
			t.Errorf("pendingBar(%d) visible width = %d; want %d", w, vw, w)
		}
	}
}

// CI usually has non-TTY stdout, term.GetSize errors and we fall back.
// either way return must be (0, tuiDisplayWidth]
func TestTermWidthCapsAt80(t *testing.T) {
	w := termWidth()
	if w <= 0 || w > tuiDisplayWidth {
		t.Errorf("termWidth() = %d; expected (0, %d]", w, tuiDisplayWidth)
	}
}

// -od dedup frame shows the brief "reading index" indicator (library keys
// pulled from the sorted sidecars), replacing the old routing phase.
func TestRenderDedupShowsLibraryReadProgress(t *testing.T) {
	m := &ulpengine.Metrics{}
	r := &ulpengine.Resolved{TotalInputs: 1, Workers: 1, DedupWorkers: 1, BucketCount: 4}
	r.Cfg.DestDedup = true
	r.OdMetrics = &ulpengine.ODMetrics{}
	r.OdMetrics.KeysTotalEstimate.Store(1000)
	r.OdMetrics.KeysLoaded.Store(250)

	out := strings.Join(renderDedupLines(time.Now(), time.Second, m, r, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(out, "matching") || !strings.Contains(out, "loaded") {
		t.Fatalf("dedup frame missing library matching indicator:\n%s", out)
	}
	if !strings.Contains(out, "250") || !strings.Contains(out, "1,000") {
		t.Fatalf("dedup frame missing read counts:\n%s", out)
	}

	// large libraries use full comma counts, not compact B suffix
	r.OdMetrics.KeysTotalEstimate.Store(3_320_076_168)
	r.OdMetrics.KeysLoaded.Store(2_370_000_000)
	outLarge := strings.Join(renderDedupLines(time.Now(), time.Second, m, r, 0, 0, 0, 0, 86), "\n")
	for _, want := range []string{"2,370,000,000", "3,320,076,168"} {
		if !strings.Contains(outLarge, want) {
			t.Fatalf("dedup frame missing full count %q:\n%s", want, outLarge)
		}
	}
	for _, ln := range strings.Split(outLarge, "\n") {
		if !strings.Contains(ln, "matching") {
			continue
		}
		if strings.Contains(ln, "B") && !strings.Contains(ln, "Library") {
			t.Fatalf("library matching row should not use compact counts:\n%s", ln)
		}
	}

	// non-od dedup must NOT show the library row
	plain := strings.Join(renderDedupLines(time.Now(), time.Second, m,
		&ulpengine.Resolved{TotalInputs: 1, Workers: 1, DedupWorkers: 1, BucketCount: 4}, 0, 0, 0, 0, 86), "\n")
	if strings.Contains(plain, "Library") {
		t.Fatalf("non-od dedup should not show Library row:\n%s", plain)
	}
}

func TestRenderLibraryMatchingRowsSingleLineAtDefaultWidth(t *testing.T) {
	done, total := int64(2_370_000_000), int64(3_320_076_168)
	innerW := boxInnerWidth(86)
	rows := renderLibraryMatchingRows(done, total, innerW)
	if len(rows) != 1 {
		t.Fatalf("want 1 row at width 86 inner=%d, got %d:\n%s", innerW, len(rows), strings.Join(rows, "\n"))
	}
	joined := rows[0]
	for _, want := range []string{"matching", "2,370,000,000", "3,320,076,168", "loaded"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
}

func TestRenderLibraryMatchingRowsStacksWhenNarrow(t *testing.T) {
	done, total := int64(2_370_000_000), int64(3_320_076_168)
	innerW := boxInnerWidth(60)
	rows := renderLibraryMatchingRows(done, total, innerW)
	if len(rows) != 2 {
		t.Fatalf("want 2 stacked rows at innerW=%d, got %d:\n%s", innerW, len(rows), strings.Join(rows, "\n"))
	}
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"Library", "matching", "2,370,000,000", "3,320,076,168", "loaded"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in stacked layout:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "…") {
		t.Errorf("stacked layout must not truncate with ellipsis:\n%s", joined)
	}
}

func TestRenderDedupLibraryRowFitsWidthWithOD(t *testing.T) {
	m := &ulpengine.Metrics{}
	r := &ulpengine.Resolved{
		Cfg:            ulpengine.Config{Output: "out.txt.zst", Compress: true, DestDedup: true},
		TotalInputs:    18 << 30,
		InputFileCount: 1,
		Workers:        8,
		DedupWorkers:   4,
		BucketCount:    256,
	}
	r.OdMetrics = &ulpengine.ODMetrics{}
	r.OdMetrics.KeysTotalEstimate.Store(3_320_076_168)
	r.OdMetrics.KeysLoaded.Store(2_370_000_000)
	for _, w := range []int{86, 60} {
		lines := renderDedupLines(time.Now(), 48*time.Second, m, r, 290.1, 410.0, 240e6, 0, w)
		for _, ln := range lines {
			// header badges can exceed width at 60 cols; box + bars must fit
			if !strings.Contains(ln, "│") && !strings.Contains(ln, "Parsing") && !strings.Contains(ln, "Deduping") {
				continue
			}
			if vw := tuiVisibleWidth(ln); vw > w {
				t.Errorf("width=%d line visible width %d > %d: %q", w, vw, w, ln)
			}
		}
	}
}
