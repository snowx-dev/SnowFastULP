package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// each part gets its own .idx in idxSubdirName, no aggregation
func TestRegenPartsMultiPartProducesPerPartSidecars(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260101-000000", 4, 200)

	m := &odMetrics{}
	_, err := regenParts(context.Background(), parts, odConfig{Workers: 4}, m)
	if err != nil {
		t.Fatal(err)
	}

	if got := m.partsRegenDone.Load(); got != 4 {
		t.Errorf("partsRegenDone = %d, want 4", got)
	}
	if got := m.archivesRegenedDone.Load(); got != 4 {
		t.Errorf("archivesRegenedDone = %d, want 4 (per-part counter)", got)
	}
	for _, p := range parts {
		hdr, err := readSidecarHeader(p.sidecarPath)
		if err != nil {
			t.Fatalf("sidecar read failed for %s: %v", filepath.Base(p.path), err)
		}
		if hdr.keyCount < 200 {
			t.Errorf("part %s keyCount = %d, want >= 200", filepath.Base(p.path), hdr.keyCount)
		}
	}
	// scratch files cleaned up after finalize
	matches, _ := filepath.Glob(filepath.Join(dir, "*.regen-part.*.tmp"))
	if len(matches) != 0 {
		t.Errorf("per-part scratch logs not cleaned up: %v", matches)
	}
}

// clean run: partsRegenDone == input parts exactly. guards vs worker
// silently skipping a part = TUI freezes at 99%
func TestRegenPartsCounterFinalizesEqualsTotal(t *testing.T) {
	dir := t.TempDir()
	const partsPerRun = 3
	const runs = 2
	var allParts []archivePart
	for r := 0; r < runs; r++ {
		runID := fmt.Sprintf("sfu_20260101-00000%d", r)
		ps := buildParts(t, dir, runID, partsPerRun, 100)
		allParts = append(allParts, ps...)
	}

	m := &odMetrics{}
	m.partsRegenTotal.Store(int32(len(allParts)))
	_, err := regenParts(context.Background(), allParts, odConfig{Workers: 4}, m)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.partsRegenDone.Load(); got != int32(len(allParts)) {
		t.Errorf("partsRegenDone = %d, want %d", got, len(allParts))
	}
}

// corrupt part must not poison siblings. main UX gain vs old per-run
// model that skipped a whole run for one bad part
func TestRegenPartsCorruptPartSkipsOnlyItself(t *testing.T) {
	dir := t.TempDir()
	goodParts := buildParts(t, dir, "sfu_20260101-000001_good", 2, 50)

	// non-zstd file w/ .zst ext, decode err = errCorruptArchive
	badPath := filepath.Join(dir, "sfu_20260101-000002_bad_part1.txt.zst")
	if err := os.WriteFile(badPath, []byte("not zstd data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPart := archivePart{path: badPath, partNum: 1, size: 14, sidecarPath: sidecarPathForArchive(badPath)}

	allParts := append([]archivePart(nil), goodParts...)
	allParts = append(allParts, badPart)

	// silence the noisy warning in test output
	origStderr := os.Stderr
	devnull, _ := os.Open(os.DevNull)
	os.Stderr = devnull
	defer func() { os.Stderr = origStderr; devnull.Close() }()

	m := &odMetrics{}
	skippedPaths, err := regenParts(context.Background(), allParts, odConfig{Workers: 2}, m)
	if err != nil {
		t.Fatalf("regenParts returned err: %v", err)
	}

	if len(skippedPaths) != 1 || skippedPaths[0] != badPath {
		t.Errorf("skipped paths = %v, want [%s]", skippedPaths, badPath)
	}
	if m.archivesRegenedDone.Load() != int32(len(goodParts)) {
		t.Errorf("archivesRegenedDone = %d, want %d", m.archivesRegenedDone.Load(), len(goodParts))
	}
	for _, p := range goodParts {
		if _, err := os.Stat(p.sidecarPath); err != nil {
			t.Errorf("good part's sidecar missing: %v", err)
		}
	}
	if _, err := os.Stat(badPart.sidecarPath); err == nil {
		t.Errorf("bad part's sidecar should not have been written")
	}
}

// touch 1 part of multi-part = only that part stale, siblings fresh.
// headline UX win vs old per-run model
func TestPerPartStalenessIsolation(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260201-aaa", 4, 100)

	m := &odMetrics{}
	if _, err := regenParts(context.Background(), parts, odConfig{Workers: 4}, m); err != nil {
		t.Fatal(err)
	}

	// snapshot modtimes to detect untouched ones
	var stamps []time.Time
	for _, p := range parts {
		fi, err := os.Stat(p.sidecarPath)
		if err != nil {
			t.Fatalf("sidecar missing for %s: %v", filepath.Base(p.path), err)
		}
		stamps = append(stamps, fi.ModTime())
	}

	// touch part 2 to invalidate just its sidecar
	time.Sleep(20 * time.Millisecond) // mtime resolution guard
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(parts[1].path, future, future); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(parts[1].path)
	parts[1].modTime = fi.ModTime()
	for i := range parts {
		fi, _ := os.Stat(parts[i].path)
		parts[i].modTime = fi.ModTime()
	}

	// only part 2 stale
	for i, p := range parts {
		st, _ := classifyPartSidecar(p)
		switch i {
		case 1:
			if st != sidecarStatusStale {
				t.Errorf("part 2 should be stale, got %v", st)
			}
		default:
			if st != sidecarStatusFresh {
				t.Errorf("part %d should be fresh, got %v", i+1, st)
			}
		}
	}
}

// orphan .idx (no matching .zst) gets removed, live ones stay
func TestSweepOrphanedSidecars(t *testing.T) {
	dir := t.TempDir()
	if err := ensureIdxSubdir(dir); err != nil {
		t.Fatal(err)
	}

	// 2 .idx w/ matching archives + 1 orphan
	mkIdx := func(name string) string {
		archive := filepath.Join(dir, name)
		if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		idxPath := sidecarPathForArchive(archive)
		if err := os.WriteFile(idxPath, []byte("dummy idx"), 0o600); err != nil {
			t.Fatal(err)
		}
		return archive
	}
	liveA := mkIdx("sfu_a_part1.txt.zst")
	liveB := mkIdx("sfu_b_part1.txt.zst")

	// orphan: no matching .zst
	orphanPath := filepath.Join(dir, idxSubdirName, "sfu_ghost_part1.txt.zst.idx")
	if err := os.WriteFile(orphanPath, []byte("orphaned"), 0o600); err != nil {
		t.Fatal(err)
	}

	runs, err := discoverArchiveRuns(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	removed, err := sweepOrphanedSidecars(dir, runs)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Errorf("orphan should be gone, got err=%v", err)
	}
	for _, p := range []string{liveA, liveB} {
		if _, err := os.Stat(sidecarPathForArchive(p)); err != nil {
			t.Errorf("live sidecar removed by sweep: %v", err)
		}
	}
}

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

// first -od run = no subdir yet, sweep must be no-op
func TestSweepOrphanedSidecarsMissingSubdir(t *testing.T) {
	dir := t.TempDir()
	removed, err := sweepOrphanedSidecars(dir, nil)
	if err != nil {
		t.Fatalf("missing subdir should not error: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

// 1 task = decoder consumes all GOMAXPROCS. indirect, just runs to done
func TestRegenPartsSinglePartUsesAllDecoderConcurrency(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260101-000003_single", 1, 50)

	m := &odMetrics{}
	_, err := regenParts(context.Background(), parts, odConfig{}, m)
	if err != nil {
		t.Fatal(err)
	}
	if m.partsRegenDone.Load() != 1 {
		t.Errorf("partsRegenDone = %d, want 1", m.partsRegenDone.Load())
	}
}

// count zstd parts in dir, each w/ lines synthetic creds, sorted by partNum
func buildParts(t *testing.T, dir, runID string, count, lines int) []archivePart {
	t.Helper()
	out := make([]archivePart, count)
	for i := 0; i < count; i++ {
		var raw bytes.Buffer
		for l := 0; l < lines; l++ {
			fmt.Fprintf(&raw, "https://site%d.example.com/login:user%d_%d:password%d\n", i, i, l, l)
		}
		path := filepath.Join(dir, fmt.Sprintf("%s_part%d.txt.zst", runID, i+1))
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w, err := zstd.NewWriter(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(raw.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(path)
		out[i] = archivePart{
			path:        path,
			partNum:     i + 1,
			size:        st.Size(),
			modTime:     st.ModTime(),
			sidecarPath: sidecarPathForArchive(path),
		}
	}
	return out
}

// parts denominator (ticks) wins over archives when both populated
func TestLibraryRowShowsPartsProgressDuringRegen(t *testing.T) {
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseRegen))
	m.archivesTotal.Store(1)
	m.filesTotal.Store(16)
	m.archivesNeedRegen.Store(1)
	m.partsRegenTotal.Store(16)
	m.partsRegenDone.Store(7)
	m.regenBytesTotal.Store(34 * 1024 * 1024 * 1024)
	m.regenBytesRead.Store(15 * 1024 * 1024 * 1024)

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
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseRegen))
	m.archivesTotal.Store(3)
	m.archivesNeedRegen.Store(3)
	m.archivesRegenedDone.Store(1)
	// partsRegenTotal intentionally 0

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if !strings.Contains(out, "1 / 3 indexing") {
		t.Errorf("missing fallback archive-grained label\nout:\n%s", out)
	}
}

// after routing, bucket files still flushing/closing. say so explicitly,
// not "stuck at 100%"
func TestRenderODFrameShowsBucketCommitPhase(t *testing.T) {
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseCommitBuckets))
	m.archivesTotal.Store(1)
	m.filesTotal.Store(16)
	m.keysTotalEstimate.Store(1000)
	m.keysLoaded.Store(1000)

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if !strings.Contains(out, "committing lookup buckets") {
		t.Errorf("missing commit phase label\nout:\n%s", out)
	}
	if !strings.Contains(out, "Entries") || !strings.Contains(out, "1,000") {
		t.Errorf("commit phase should keep final entry count visible\nout:\n%s", out)
	}
}

// worker mini-bars must sit outside the gradientBox, indented to match
// the main progress bar
func TestWorkerBarsRenderedOutsideFrame(t *testing.T) {
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseRegen))
	m.archivesTotal.Store(1)
	m.archivesNeedRegen.Store(1)
	m.regenBytesTotal.Store(1 << 30)
	m.regenBytesRead.Store(1 << 28)
	m.workers = make([]workerStatus, 1)
	name := "sfu_test_part1.txt.zst"
	m.workers[0].archivePath.Store(&name)
	m.workers[0].partIdx.Store(1)
	m.workers[0].partsTotal.Store(1)
	m.workers[0].bytesDone.Store(1 << 27)
	m.workers[0].bytesTotal.Store(1 << 28)

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

// runODScan returns the sorted library sidecar paths synchronously (no routing
// into scratch). dedup reads each bucket's keys from them lazily.
func TestRunODScanReturnsSidecarPaths(t *testing.T) {
	dir := t.TempDir()
	tempDir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260101-000000_def", 1, 200)
	regenSidecarForTest(t, parts[0].path)

	m := &odMetrics{}
	res, err := runODScan(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_other",
		Buckets:         4,
		TempDir:         tempDir,
	}, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.ArchivesTotal != 1 {
		t.Errorf("ArchivesTotal = %d, want 1", res.ArchivesTotal)
	}
	if len(res.DestSidecarPaths) != 1 {
		t.Fatalf("DestSidecarPaths = %v, want 1 sidecar", res.DestSidecarPaths)
	}
	if res.TotalKeysLoaded == 0 {
		t.Errorf("TotalKeysLoaded should be > 0")
	}
	// phase-0 work is done synchronously; the gather happens during dedup.
	if got := odPhase(m.phase.Load()); got != odPhaseDone {
		t.Errorf("phase after scan = %v, want odPhaseDone", got)
	}
}

// re-streams part + writes fresh .idx, skips the worker pool
func regenSidecarForTest(t *testing.T, partPath string) {
	t.Helper()
	sw, err := newSidecarWriter(partPath)
	if err != nil {
		t.Fatal(err)
	}
	fmtr := newLineFormatter()
	if err := streamArchiveLines(context.Background(), partPath, 1, nil, func(line string) error {
		host, _, login, password, ok := parseFor(line, true)
		if !ok {
			return nil
		}
		return sw.WriteHash(fmtr.HashKey(host, login, password))
	}, nil); err != nil {
		_ = sw.Abort()
		t.Fatal(err)
	}
	if _, err := sw.Commit(); err != nil {
		t.Fatal(err)
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
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseRegen))
	m.archivesTotal.Store(1)
	m.archivesNeedRegen.Store(1)
	m.regenBytesTotal.Store(1 << 40)
	m.workers = make([]workerStatus, 3)

	// rows w/ very different name widths
	names := []string{
		"sfu_a_part1.txt.zst",
		"sfu_some_longer_archive_id_part12.txt.zst",
		"sfu_xy_part2.txt.zst",
	}
	for i := range m.workers {
		n := names[i]
		m.workers[i].archivePath.Store(&n)
		m.workers[i].partIdx.Store(int32(i + 1))
		m.workers[i].partsTotal.Store(16)
		m.workers[i].bytesDone.Store(int64(i) * (1 << 28))
		m.workers[i].bytesTotal.Store(1 << 30)
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

// post-dedup own-output indexing: one .idx per part, drives phaseIndex
// counters, lands on odPhaseDone. regression for "dedup bar frozen at 100%"
func TestRegenOwnOutputSidecarsParallel(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_20260514-000000", 4, 50)
	outputPaths := make([]string, len(parts))
	for i, p := range parts {
		outputPaths[i] = p.path
	}

	m := &odMetrics{}
	if err := regenOwnOutputSidecars(context.Background(), outputPaths, nil, m); err != nil {
		t.Fatalf("regenOwnOutputSidecars: %v", err)
	}

	if got := odPhase(m.phase.Load()); got != odPhaseDone {
		t.Errorf("post-call phase = %v, want odPhaseDone", got)
	}
	if got := m.partsRegenTotal.Load(); got != int32(len(parts)) {
		t.Errorf("partsRegenTotal = %d, want %d", got, len(parts))
	}
	if got := m.partsRegenDone.Load(); got != int32(len(parts)) {
		t.Errorf("partsRegenDone = %d, want %d", got, len(parts))
	}
	if got := m.regenBytesRead.Load(); got != m.regenBytesTotal.Load() {
		t.Errorf("bytesRead=%d != bytesTotal=%d (bar would never hit 100%%)",
			got, m.regenBytesTotal.Load())
	}
	for _, p := range parts {
		if _, err := os.Stat(p.sidecarPath); err != nil {
			t.Errorf("missing sidecar for %s: %v", filepath.Base(p.path), err)
		}
	}
}

// no-op on empty input. guards vs div-by-zero in worker sizing for
// 0-part output (fast-path on zero-line input)
func TestRegenOwnOutputSidecarsEmpty(t *testing.T) {
	m := &odMetrics{}
	if err := regenOwnOutputSidecars(context.Background(), nil, nil, m); err != nil {
		t.Fatalf("empty call should be a no-op, got err: %v", err)
	}
	// phase counters untouched, nothing to surface
	if got := odPhase(m.phase.Load()); got != odPhaseIdle {
		t.Errorf("phase = %v, want odPhaseIdle", got)
	}
}

// phaseIndex rebrand: "Output index" not "Destination dedup".
// worker rows + byte progress still render
func TestRenderODFrameSwitchesToOutputIndexHeader(t *testing.T) {
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseIndexOwn))
	m.archivesTotal.Store(1)
	m.filesTotal.Store(4)
	m.archivesNeedRegen.Store(1)
	m.partsRegenTotal.Store(4)
	m.partsRegenDone.Store(2)
	m.regenBytesTotal.Store(1 << 30)
	m.regenBytesRead.Store(1 << 28)
	m.workers = make([]workerStatus, 1)
	name := "sfu_run_part1.txt.zst"
	m.workers[0].archivePath.Store(&name)
	m.workers[0].partIdx.Store(1)
	m.workers[0].partsTotal.Store(1)
	m.workers[0].bytesDone.Store(1 << 27)
	m.workers[0].bytesTotal.Store(1 << 28)

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
	odM := &odMetrics{}
	odM.phase.Store(int32(odPhaseDone))
	odM.regenBytesTotal.Store(99 * 1024 * 1024 * 1024)
	odM.regenBytesRead.Store(99 * 1024 * 1024 * 1024)

	outM := &odMetrics{}
	outM.phase.Store(int32(odPhaseIndexOwn))
	outM.archivesTotal.Store(1)
	outM.filesTotal.Store(4)
	outM.partsRegenTotal.Store(4)
	outM.partsRegenDone.Store(1)
	outM.regenBytesTotal.Store(1 << 30)
	outM.regenBytesRead.Store(1 << 28)

	r := &resolved{odMetrics: odM, outputIdxMetrics: outM}
	m := &metrics{}
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
	workers := make([]workerStatus, total)
	for i := range workers {
		name := fmt.Sprintf("sfu_run_part%d.txt.zst", i+1)
		workers[i].archivePath.Store(&name)
		workers[i].partIdx.Store(int32(i + 1))
		workers[i].partsTotal.Store(total)
		// keep bytesDone < 1 GB so humanBytes stays at MB (7-8 chars),
		// else dynamic rightW shifts bar column for variable totals
		workers[i].bytesDone.Store(int64(i+1) * 50 * 1024 * 1024)
		workers[i].bytesTotal.Store(2 * 1024 * 1024 * 1024)
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

// race-detector probe: writers mutate slots while reader iterates
func TestWorkerStatusConcurrentReadWrite(t *testing.T) {
	m := &odMetrics{}
	m.workers = make([]workerStatus, 8)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := range m.workers {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				name := fmt.Sprintf("part%d_%d", idx, n)
				m.workers[idx].archivePath.Store(&name)
				m.workers[idx].bytesDone.Add(1)
				n++
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.activeWorkers(16)
		}
	}()

	// race them
	for i := 0; i < 1000; i++ {
		_ = m.activeWorkers(16)
	}
	close(stop)
	wg.Wait()
}
