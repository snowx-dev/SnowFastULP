package search

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/snowx-dev/SnowFastULP/internal/index"
)

func TestLineAssemblerMatchAllSingleBuffer(t *testing.T) {
	var a lineAssembler
	hits, _ := a.feed(nil, []byte("alpha\n\nbeta\r\n"), 0, matchAllRegion, -1)
	hits, _ = a.flush(hits, matchAllRegion, -1)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (empty line skipped)", len(hits))
	}
	if hits[0].line != "alpha" || hits[1].line != "beta" {
		t.Fatalf("lines = %q, %q", hits[0].line, hits[1].line)
	}
}

func TestLineAssemblerMatchAllSplitAcrossSteps(t *testing.T) {
	var a lineAssembler
	hits, _ := a.feed(nil, []byte("hel"), 0, matchAllRegion, -1)
	hits, _ = a.feed(hits, []byte("lo\nworld\n"), 3, matchAllRegion, -1)
	hits, _ = a.flush(hits, matchAllRegion, -1)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].line != "hello" || hits[1].line != "world" {
		t.Fatalf("lines = %q, %q", hits[0].line, hits[1].line)
	}
	if hits[0].offset != 0 || hits[1].offset != 6 {
		t.Fatalf("offsets = %d,%d want 0,6", hits[0].offset, hits[1].offset)
	}
}

func TestLineAssemblerFlushTrailingLine(t *testing.T) {
	var a lineAssembler
	hits, _ := a.feed(nil, []byte("tail"), 42, matchAllRegion, -1)
	hits, _ = a.flush(hits, matchAllRegion, -1)
	if len(hits) != 1 || hits[0].line != "tail" || hits[0].offset != 42 {
		t.Fatalf("hit = %+v", hits)
	}
}

// A pattern match whose line straddles a feed seam must be emitted once, whole,
// with the offset at the true match position — and not before the line completes.
func TestLineAssemblerPatternAcrossSteps(t *testing.T) {
	m := newPatternMatcher([]byte("KEY"))
	proc := patternRegion(&m)
	var a lineAssembler
	hits, _ := a.feed(nil, []byte("abK"), 0, proc, -1) // line not terminated yet
	if len(hits) != 0 {
		t.Fatalf("emitted before line completed: %+v", hits)
	}
	hits, _ = a.feed(hits, []byte("EYcd\n"), 3, proc, -1)
	hits, _ = a.flush(hits, proc, -1)
	if len(hits) != 1 || hits[0].line != "abKEYcd" {
		t.Fatalf("hit = %+v, want line abKEYcd", hits)
	}
	if hits[0].offset != 2 {
		t.Fatalf("offset = %d, want 2 (true match position)", hits[0].offset)
	}
}

// A newline-free run longer than maxLineBytes is matched on its head, truncated,
// then the assembler resyncs at the next newline and resumes normal matching —
// bounding memory without losing later lines.
func TestLineAssemblerOverflowTruncatesAndResyncs(t *testing.T) {
	m := newPatternMatcher([]byte("HEAD"))
	proc := patternRegion(&m)
	var a lineAssembler

	big := bytes.Repeat([]byte("x"), maxLineBytes+1000) // no newline
	copy(big[10:], []byte("HEAD"))
	hits, _ := a.feed(nil, big, 0, proc, -1)
	if len(hits) != 1 || hits[0].offset != 10 {
		t.Fatalf("overflow head: hits = %d (want 1 at offset 10)", len(hits))
	}
	if len(hits[0].line) != maxLineBytes {
		t.Fatalf("truncated head len = %d, want %d", len(hits[0].line), maxLineBytes)
	}

	// remainder of the over-long line, then a clean matchable line
	hits, _ = a.feed(hits, []byte("yy\nfoo:HEAD:bar\n"), int64(len(big)), proc, -1)
	hits, _ = a.flush(hits, proc, -1)
	if len(hits) != 2 || hits[1].line != "foo:HEAD:bar" {
		t.Fatalf("after resync: hits = %+v", hits)
	}
}

func TestRunTxtMatchAllSkipsEmptyLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(p, []byte("one\n\n two \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtMatchAllCollect(t, p)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Line != "one" || hits[1].Line != " two " {
		t.Fatalf("lines = %q, %q", hits[0].Line, hits[1].Line)
	}
}

func TestRunTxtMatchAllStraddlingReadBoundary(t *testing.T) {
	const step = 1 << 20
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	pre := step - 5
	body := bytes.Repeat([]byte("a"), pre)
	body = append(body, []byte("line1\nline2\n")...)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtMatchAllCollect(t, p)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if !strings.HasSuffix(hits[0].Line, "line1") || hits[1].Line != "line2" {
		t.Fatalf("lines = %q, %q", hits[0].Line, hits[1].Line)
	}
}

func TestRunTxtMatchAllNoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tail.txt")
	if err := os.WriteFile(p, []byte("only-line"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtMatchAllCollect(t, p)
	if len(hits) != 1 || hits[0].Line != "only-line" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestRunMatchAllZstSplitDecodeStep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "split.zst")
	body := []byte("part1-part2\n")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan Hit, 8)
	err = Run(Config{
		MatchAll:   true,
		DecodeStep: 4,
		Workers:    1,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		Hits:       hitCh,
		ArchiveOrd: map[string]int{path: 0},
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}
	var hits []Hit
	for h := range hitCh {
		hits = append(hits, h)
	}
	if len(hits) != 1 || hits[0].Line != "part1-part2" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestRunMatchAllMaxHitsPerChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.zst")
	body := bytes.Repeat([]byte("line\n"), 100)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan Hit, 64)
	var got int
	err = Run(Config{
		MatchAll:        true,
		Workers:         1,
		Archives:        []string{path},
		Sidecars:        map[string]*index.Sidecar{path: sc},
		Hits:            hitCh,
		ArchiveOrd:      map[string]int{path: 0},
		MaxHitsPerChunk: 10,
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}
	for range hitCh {
		got++
	}
	if got != 10 {
		t.Fatalf("hits = %d, want 10 (cap)", got)
	}
}

func TestRunMatchAllEmptyPatternGuard(t *testing.T) {
	err := Run(Config{Pattern: []byte{}})
	if err == nil || !strings.Contains(err.Error(), "empty pattern") {
		t.Fatalf("err = %v", err)
	}
	err = Run(Config{MatchAll: true})
	if err != nil {
		t.Fatalf("MatchAll without pattern should run: %v", err)
	}
	err = Run(Config{MultiMatcher: NewMultiMatcher([][]byte{[]byte("foo")})})
	if err != nil {
		t.Fatalf("MultiMatcher with empty Pattern should run: %v", err)
	}
	err = RunTxt(TxtConfig{MultiMatcher: NewMultiMatcher([][]byte{[]byte("foo")})})
	if err != nil {
		t.Fatalf("RunTxt MultiMatcher with empty Pattern should run: %v", err)
	}
}

func runTxtMatchAllCollect(t *testing.T, path string) []Hit {
	t.Helper()
	hitCh := make(chan Hit, 64)
	err := RunTxt(TxtConfig{
		Ctx:        context.Background(),
		MatchAll:   true,
		Workers:    1,
		Files:      []string{path},
		ArchiveOrd: map[string]int{path: 0},
		Hits:       hitCh,
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}
	var hits []Hit
	for h := range hitCh {
		hits = append(hits, h)
	}
	return hits
}

// Per-matching-line semantics (grep): one line, one hit per pattern. "foo"
// matches the line twice and "bar" once — the two foo occurrences collapse to
// ONE hit (same pattern, same line), while bar is a different pattern and
// emits its own hit, so the line yields exactly 2 hits.
func TestMultiPatternRegionEmitsPerMatchingLine(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("foo"), []byte("bar")})
	proc := multiPatternRegion(m)
	region := []byte("xfoo ybar fooz\n")
	hits, _ := proc(nil, region, 0, -1)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (foo deduped, bar separate): %+v", len(hits), hits)
	}
	want := []struct {
		offset int64
		idx    int
		line   string
	}{{1, 0, "xfoo ybar fooz"}, {6, 1, "xfoo ybar fooz"}}
	for i, w := range want {
		if hits[i].offset != w.offset || hits[i].patternIdx != w.idx || hits[i].line != w.line {
			t.Fatalf("hit %d = %+v, want %+v", i, hits[i], w)
		}
	}
}

func TestMultiPatternRegionEmptyRegion(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("foo")})
	proc := multiPatternRegion(m)
	hits, _ := proc(nil, []byte(""), 0, -1)
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits, got %d", len(hits))
	}
}

// Per-matching-line semantics: a line matching the same pattern at several
// positions emits ONE hit, carrying the FIRST occurrence's offset.
func TestPatternRegionEmitsPerMatchingLine(t *testing.T) {
	m := newPatternMatcher([]byte("@"))
	proc := patternRegion(&m)
	hits, _ := proc(nil, []byte("a@b@c\n"), 10, -1)
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (per line, not per occurrence): %+v", len(hits), hits)
	}
	if hits[0].offset != 11 || hits[0].line != "a@b@c" {
		t.Fatalf("hit = %+v, want offset 11 (FIRST occurrence) line a@b@c", hits[0])
	}
}

// Two byte-identical lines in one region are DISTINCT lines: each emits its
// own hit (dedupe is by line offset, never by line content).
func TestPatternRegionIdenticalLinesEmitPerLine(t *testing.T) {
	m := newPatternMatcher([]byte("k"))
	proc := patternRegion(&m)
	region := []byte("kx\nkx\n")
	hits, _ := proc(nil, region, 100, -1)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (identical texts, distinct lines): %+v", len(hits), hits)
	}
	if hits[0].offset != 100 || hits[1].offset != 103 {
		t.Fatalf("offsets = %d,%d want 100,103", hits[0].offset, hits[1].offset)
	}
	if hits[0].line != "kx" || hits[1].line != "kx" {
		t.Fatalf("lines = %q,%q", hits[0].line, hits[1].line)
	}
}

// A pattern containing a raw newline (multi-line span) covers several lines;
// every covered line emits its own hit — the scan must resume after the LAST
// line of the span, not the first, so tail lines still emit.
func TestPatternRegionMultiLineSpanEmitsEachCoveredLine(t *testing.T) {
	m := newPatternMatcher([]byte("x\ny"))
	proc := patternRegion(&m)
	// 3-line region: the span "x\ny" covers line 1 ("x") and line 2 ("y");
	// a third match on line 3 must still be found.
	region := []byte("x\ny\nz\nx\ny\nz\n")
	hits, _ := proc(nil, region, 0, -1)
	if len(hits) != 4 {
		t.Fatalf("hits = %d, want 4 (x,y per span, plus third-line z... x,y again): %+v", len(hits), hits)
	}
	// Offsets at the span START of each covered line, in order.
	want := []struct {
		offset int64
		line   string
	}{{0, "x"}, {2, "y"}, {6, "x"}, {8, "y"}}
	for i, w := range want {
		if hits[i].offset != w.offset || hits[i].line != w.line {
			t.Fatalf("hit %d = %+v, want offset %d line %q", i, hits[i], w.offset, w.line)
		}
	}
}

// extractLine can return "" (a bare-"\r" line whose stripped body is empty) —
// the hit still emits and the scan still advances past that line: no stall,
// later lines keep emitting.
func TestPatternRegionEmptyStrippedLineDoesNotStall(t *testing.T) {
	m := newPatternMatcher([]byte("\r"))
	proc := patternRegion(&m)
	// Line 1 is "\r": extractLine strips the CR and returns "". Lines 2-3
	// must still emit.
	region := []byte("\r\nx\r\nfoo\r\n")
	hits, _ := proc(nil, region, 0, -1)
	if len(hits) != 3 {
		t.Fatalf("hits = %d, want 3 (empty stripped line consumed, later lines emit): %+v", len(hits), hits)
	}
	if hits[0].offset != 0 || hits[0].line != "" || hits[1].offset != 3 || hits[1].line != "x" || hits[2].offset != 8 || hits[2].line != "foo" {
		t.Fatalf("hits = %+v, want empty line at 0, x at 3, foo at 8 (first-occurrence offsets)", hits)
	}
}

// Per-line pattern semantics: a multi-pattern line match must emit one hit
// per (pattern, line). foo matches twice on the line, bar once — foo emits
// once (first occurrence offset), bar once.
func TestMultiPatternRegionLineSpansSeam(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("needle")})
	proc := multiPatternRegion(m)
	var a lineAssembler
	hits, _ := a.feed(nil, []byte("alpha nee"), 0, proc, -1)
	hits, _ = a.feed(hits, []byte("dle beta\n"), 9, proc, -1)
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (line assembled across seam)", len(hits))
	}
	if hits[0].patternIdx != 0 || hits[0].line != "alpha needle beta" {
		t.Fatalf("hit = %+v", hits[0])
	}
	if hits[0].offset != 6 {
		t.Fatalf("offset = %d, want 6 (first occurrence)", hits[0].offset)
	}
}

// A line matching the same multi pattern twice emits ONE hit; two identical
// line texts in one region are distinct lines and both emit. A line matching
// two different patterns emits one hit per pattern.
func TestMultiPatternRegionIdenticalLinesAndPatterns(t *testing.T) {
	m := NewMultiMatcher([][]byte{[]byte("foo"), []byte("bar")})
	proc := multiPatternRegion(m)
	// line 1: foo twice + bar → 2 hits (idx0 deduped + idx1)
	// line 2: identical text to line 1 → 2 more hits (distinct line offset)
	region := []byte("xfoo bar fooz\nxfoo bar fooz\n")
	hits, _ := proc(nil, region, 0, -1)
	if len(hits) != 4 {
		t.Fatalf("hits = %d, want 4 (2 per identical line): %+v", len(hits), hits)
	}
	// line 1 in patternIdx order at same pos? EachMatchUntil walks by end
	// position; assert per-line pattern sets and first-occurrence offsets.
	type key struct {
		off int64
		idx int
	}
	got := map[key]bool{}
	for _, h := range hits {
		got[key{h.offset, h.patternIdx}] = true
	}
	for _, w := range []key{{1, 0}, {5, 1}, {15, 0}, {19, 1}} {
		if !got[w] {
			t.Fatalf("missing hit offset=%d patternIdx=%d in %+v", w.off, w.idx, hits)
		}
	}
}
