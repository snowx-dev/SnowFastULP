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
	hits := a.feed(nil, []byte("alpha\n\nbeta\r\n"), 0, matchAllRegion)
	hits = a.flush(hits, matchAllRegion)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (empty line skipped)", len(hits))
	}
	if hits[0].line != "alpha" || hits[1].line != "beta" {
		t.Fatalf("lines = %q, %q", hits[0].line, hits[1].line)
	}
}

func TestLineAssemblerMatchAllSplitAcrossSteps(t *testing.T) {
	var a lineAssembler
	hits := a.feed(nil, []byte("hel"), 0, matchAllRegion)
	hits = a.feed(hits, []byte("lo\nworld\n"), 3, matchAllRegion)
	hits = a.flush(hits, matchAllRegion)
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
	hits := a.feed(nil, []byte("tail"), 42, matchAllRegion)
	hits = a.flush(hits, matchAllRegion)
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
	hits := a.feed(nil, []byte("abK"), 0, proc) // line not terminated yet
	if len(hits) != 0 {
		t.Fatalf("emitted before line completed: %+v", hits)
	}
	hits = a.feed(hits, []byte("EYcd\n"), 3, proc)
	hits = a.flush(hits, proc)
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
	hits := a.feed(nil, big, 0, proc)
	if len(hits) != 1 || hits[0].offset != 10 {
		t.Fatalf("overflow head: hits = %d (want 1 at offset 10)", len(hits))
	}
	if len(hits[0].line) != maxLineBytes {
		t.Fatalf("truncated head len = %d, want %d", len(hits[0].line), maxLineBytes)
	}

	// remainder of the over-long line, then a clean matchable line
	hits = a.feed(hits, []byte("yy\nfoo:HEAD:bar\n"), int64(len(big)), proc)
	hits = a.flush(hits, proc)
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
