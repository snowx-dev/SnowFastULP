package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	m := &metrics{}
	base := &resolved{totalInputs: 1, workers: 1, dedupWorkers: 1, bucketCount: 1}

	noOD := strings.Join(renderShardLines(now, time.Second, m, base, 0, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(noOD, "[1/2 PARSING]") {
		t.Errorf("non-od shard: want [1/2 PARSING]\n%s", noOD)
	}
	noODDedup := strings.Join(renderDedupLines(now, time.Second, m, base, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(noODDedup, "[2/2 DEDUPING]") {
		t.Errorf("non-od dedup: want [2/2 DEDUPING]\n%s", noODDedup)
	}

	withOD := *base
	withOD.cfg.DestDedup = true
	withOD.odMetrics = &odMetrics{}
	withOD.odMetrics.phase.Store(int32(odPhaseRegen))
	withOD.outputIdxMetrics = &odMetrics{}
	withOD.outputIdxMetrics.phase.Store(int32(odPhaseRegen))

	p0 := strings.Join(renderPhase0Lines(time.Second, m, &withOD, 0, 0, 0, 86), "\n")
	if !strings.Contains(p0, "[1/3 PARSING]") {
		t.Errorf("od phase0: want [1/3 PARSING]\n%s", p0)
	}
	if strings.Contains(p0, "INDEXING LIBRARY") {
		t.Errorf("od phase0 should not have separate library header\n%s", p0)
	}
	p1 := strings.Join(renderShardLines(now, time.Second, m, &withOD, 0, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(p1, "[1/3 PARSING]") {
		t.Errorf("od shard: want [1/3 PARSING]\n%s", p1)
	}
	p2 := strings.Join(renderDedupLines(now, time.Second, m, &withOD, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(p2, "[2/3 DEDUPING]") {
		t.Errorf("od dedup: want [2/3 DEDUPING]\n%s", p2)
	}
	p3 := strings.Join(renderIndexLines(time.Second, m, &withOD, 0, 0, 0, 86), "\n")
	if !strings.Contains(p3, "[3/3 INDEXING OUTPUT]") {
		t.Errorf("od index: want [3/3 INDEXING OUTPUT]\n%s", p3)
	}
}

func TestRenderShardLinesFitsWidth(t *testing.T) {
	m := &metrics{}
	m.bytesRead.Store(1 << 30)
	m.chunksDone.Store(42)
	m.chunksTotal.Store(205)
	m.linesRead.Store(78_499_000)
	m.linesAccepted.Store(78_304_811)
	m.linesRejected.Store(194_189)
	m.busyWorkers.Store(8)
	r := &resolved{
		totalInputs:    18 << 30,
		inputFileCount: 4,
		workers:        8,
		dedupWorkers:   4,
		bucketCount:    256,
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
	m := &metrics{}
	r := &resolved{totalInputs: 1 << 30, inputFileCount: 1, workers: 4, dedupWorkers: 2, bucketCount: 64}

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
	r := &resolved{totalInputs: 18 << 30, inputFileCount: 1, workers: 8, dedupWorkers: 4, bucketCount: 256}

	mEarly := &metrics{}
	mEarly.bytesRead.Store(4 * 1 << 30) // 4 GB
	mEarly.chunksDone.Store(46)
	mEarly.chunksTotal.Store(205)
	mEarly.busyWorkers.Store(2)

	mLate := &metrics{}
	mLate.bytesRead.Store(15 * 1 << 30) // 15 GB, diff digit count
	mLate.chunksDone.Store(199)
	mLate.chunksTotal.Store(205)
	mLate.busyWorkers.Store(8)

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

// bars start at col 4, aligned w/ stat rows above
func TestRenderShardBarsAreIndented(t *testing.T) {
	m := &metrics{}
	r := &resolved{totalInputs: 1 << 30, inputFileCount: 1, workers: 4, dedupWorkers: 2, bucketCount: 64}
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
	// total visible width still 80, right edge unchanged
	if w := tuiVisibleWidth(bar1); w != 80 {
		t.Errorf("bar1 visible width = %d, want 80", w)
	}
}

func TestRenderMainProgressBarsShowPhaseLabels(t *testing.T) {
	m := &metrics{}
	r := &resolved{totalInputs: 1 << 30, inputFileCount: 1, workers: 4, dedupWorkers: 2, bucketCount: 64}
	m.bytesRead.Store(1 << 29)

	parseLines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	parseJoined := strings.Join(parseLines, "\n")
	for _, want := range []string{"Parsing", "Deduping", "----"} {
		if !strings.Contains(parseJoined, want) {
			t.Fatalf("parsing phase missing %q in:\n%s", want, parseJoined)
		}
	}

	m.bucketsTotal.Store(8)
	m.bucketsBytesTotal.Store(1000)
	m.bucketsBytesRead.Store(500)
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
	m := &metrics{}
	m.linesRead.Store(78_499_000)
	m.linesAccepted.Store(78_304_811)
	m.linesRejected.Store(194_189)
	m.bytesRead.Store(1 << 30)
	r := &resolved{totalInputs: 18 << 30, inputFileCount: 1, workers: 8, dedupWorkers: 4, bucketCount: 256}
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
	m := &metrics{}
	m.linesUnique.Store(80_000_000)
	m.linesAccepted.Store(80_234_000)
	m.bucketsDone.Store(256)
	m.bucketsTotal.Store(256)
	m.bytesWritten.Store(7 << 30)
	m.linesRejected.Store(14_000_000)
	// short relative path keeps 80/60-col asserts honest, t.TempDir
	// would bust the budget
	r := &resolved{
		cfg:            pipelineConfig{Output: "out.txt"},
		totalInputs:    18 << 30,
		inputFileCount: 3,
		workers:        8,
		dedupWorkers:   4,
		bucketCount:    256,
	}
	for _, w := range []int{80, 60} {
		now := time.Now()
		ded := renderDedupLines(now, 48*time.Second, m, r, 290.1, 410.0, 240e6, 0, w)
		done := renderFinalStdoutSummary(131*time.Second, m, r, w)
		for _, ln := range append(ded, done...) {
			if vw := tuiVisibleWidth(ln); vw > w {
				t.Errorf("width=%d line visible width %d > %d: %q", w, vw, w, ln)
			}
		}
	}
}

// DONE block surfaces every metric user expects
func TestRenderDoneIncludesAllSummaryFields(t *testing.T) {
	m := &metrics{}
	m.linesUnique.Store(77_500_000)
	m.linesAccepted.Store(77_734_000) // unique + 234,000 dups
	m.linesRejected.Store(166_172)
	m.bytesWritten.Store(8_800_000_000) // ~8.2 GB
	r := &resolved{
		cfg:            pipelineConfig{Output: "./sfu_20260509_abc123.txt"},
		totalInputs:    10_737_418_240, // 10 GB
		inputFileCount: 4,
	}
	lines := renderFinalStdoutSummary(102*time.Second, m, r, 80)
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

func TestDefaultOutputNameFormat(t *testing.T) {
	stamp := runStamp(time.Date(2026, 5, 9, 20, 35, 30, 0, time.UTC), "abc123")
	got := defaultOutputName(stamp)
	want := "sfu_20260509_abc123.txt"
	if got != want {
		t.Errorf("defaultOutputName = %q, want %q", got, want)
	}
}

// run id length + crockford alphabet only, opsec guarantee
func TestNewRunIDShape(t *testing.T) {
	const N = 256
	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		id, err := newRunID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != runIDLen {
			t.Fatalf("id %q length = %d, want %d", id, len(id), runIDLen)
		}
		for _, c := range id {
			if !strings.ContainsRune(crockfordAlphabet, c) {
				t.Fatalf("id %q contains non-alphabet char %q", id, c)
			}
		}
		seen[id] = struct{}{}
	}
	// 256 draws from 30-bit space, virtually certain to be unique
	if len(seen) < N-1 {
		t.Fatalf("got %d unique ids of %d, entropy degraded", len(seen), N)
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
		if got := withZstExt(c.in, c.compress); got != c.want {
			t.Errorf("withZstExt(%q, %v) = %q, want %q", c.in, c.compress, got, c.want)
		}
	}
}

// dedup bar prefers byte-level metric over bucket-count ratio so it
// ticks immediately, not at first bucket completion
func TestRenderDedupBarUsesByteProgress(t *testing.T) {
	m := &metrics{}
	// bucket-count says 0%, byte-count says ~50%
	m.bucketsTotal.Store(8)
	m.bucketsDone.Store(0)
	m.bucketsBytesTotal.Store(1000)
	m.bucketsBytesRead.Store(500)
	r := &resolved{
		cfg:          pipelineConfig{Output: "out.txt"},
		workers:      4,
		dedupWorkers: 2,
		bucketCount:  8,
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

// inline "compressing" badge appears iff -zst, still fits 80-col budget
func TestRenderDedupHeaderShowsCompressingBadge(t *testing.T) {
	m := &metrics{}
	m.bucketsTotal.Store(256)
	m.bucketsDone.Store(64)
	for _, compress := range []bool{false, true} {
		r := &resolved{
			cfg:          pipelineConfig{Output: "out.txt", Compress: compress},
			workers:      8,
			dedupWorkers: 4,
			bucketCount:  256,
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

// DONE block reports on-disk size + (Nx compressed) when -zst is set
func TestRenderDoneShowsCompressionRatio(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "out.txt.zst")
	sink, err := newOutputSink(out, true, false)
	if err != nil {
		t.Fatal(err)
	}
	m := &metrics{}
	// repeated line = dramatic ratio
	const N = 200
	for i := 0; i < N; i++ {
		if err := sink.writeLine("aaa.example.com:user@example.com:hunter2", m); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.close(); err != nil {
		t.Fatal(err)
	}

	r := &resolved{
		cfg:            pipelineConfig{Output: out, Compress: true},
		totalInputs:    1 << 20,
		inputFileCount: 1,
	}
	lines := renderFinalStdoutSummary(60*time.Second, m, r, 80)
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
	r := &resolved{
		cfg: pipelineConfig{Output: filepath.Join(dir, "sfu_20260509_abc123.txt.zst"), Compress: true},
		OutputPaths: []string{
			filepath.Join(dir, "sfu_20260509_abc123_part1.txt.zst"),
			filepath.Join(dir, "sfu_20260509_abc123_part2.txt.zst"),
			filepath.Join(dir, "sfu_20260509_abc123_part3.txt.zst"),
		},
	}
	lines := renderFinalStdoutSummary(time.Second, &metrics{}, r, 80)
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

func TestRenderDoneOutputFooterPlainLongPath(t *testing.T) {
	// long suffix built inline, no checked-in personal mount path
	longPath := filepath.Join(os.TempDir(), strings.Repeat("nest/", 20)+"sfu_20260509-203530.txt")
	r := &resolved{cfg: pipelineConfig{Output: longPath}}
	lines := renderFinalStdoutSummary(time.Second, &metrics{}, r, 80)
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
	m := &metrics{}
	r := &resolved{totalInputs: 1, workers: 1, dedupWorkers: 1, bucketCount: 4}
	r.cfg.DestDedup = true
	r.odMetrics = &odMetrics{}
	r.odMetrics.keysTotalEstimate.Store(1000)
	r.odMetrics.keysLoaded.Store(250)

	out := strings.Join(renderDedupLines(time.Now(), time.Second, m, r, 0, 0, 0, 0, 86), "\n")
	if !strings.Contains(out, "Library") || !strings.Contains(out, "read") {
		t.Fatalf("dedup frame missing library read indicator:\n%s", out)
	}
	if !strings.Contains(out, "250") || !strings.Contains(out, "1,000") {
		t.Fatalf("dedup frame missing read counts:\n%s", out)
	}

	// non-od dedup must NOT show the library row
	plain := strings.Join(renderDedupLines(time.Now(), time.Second, m,
		&resolved{totalInputs: 1, workers: 1, dedupWorkers: 1, bucketCount: 4}, 0, 0, 0, 0, 86), "\n")
	if strings.Contains(plain, "Library") {
		t.Fatalf("non-od dedup should not show Library row:\n%s", plain)
	}
}
