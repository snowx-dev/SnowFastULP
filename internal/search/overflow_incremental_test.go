package search

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/snowx-dev/SnowFastULP/internal/index"
)

// writeZSTFile writes body compressed as one zstd stream at path.
func writeZSTFile(t *testing.T, path string, body []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
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
}

// H-07: an over-long line whose reads CROSS maxLineBytes incrementally (the
// way real decode steps / read windows arrive) must be emitted exactly once.
// The pre-fix assembler emitted the capped head but kept the stale carry and
// never armed skip mode, so every following read — and EOF — re-processed the
// same multi-megabyte prefix: duplicate hits and massive output amplification.
func TestLineAssemblerOverflowIncrementalReadsEmitsOnce(t *testing.T) {
	m := newPatternMatcher([]byte("HEAD"))
	proc := patternRegion(&m)
	var a lineAssembler

	// carry grows below the cap across one read; the HEAD sits inside that
	// carry, so every later re-emission of the stale prefix duplicates it.
	first := bytes.Repeat([]byte("x"), 3*maxLineBytes/4)
	copy(first[100:], []byte("HEAD"))
	if hits, _ := a.feed(nil, first, 0, proc, -1); len(hits) != 0 {
		t.Fatalf("no newline yet, got %d hits", len(hits))
	}
	// ...this read crosses the cap and force-emits the capped head.
	big := bytes.Repeat([]byte("x"), maxLineBytes/4)
	hits, _ := a.feed(nil, big, 3*maxLineBytes/4, proc, -1)
	if len(hits) != 1 {
		t.Fatalf("overflow head: hits = %d, want 1", len(hits))
	}
	// ...the remainder of the over-long line must be skipped, then matching
	// resumes at the next newline. Pre-fix, the stale carry re-emitted here
	// and again at flush.
	hits, _ = a.feed(hits, bytes.Repeat([]byte("y"), maxLineBytes/2), maxLineBytes, proc, -1)
	hits, _ = a.flush(hits, proc, -1)
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (capped head emitted once, no duplicates)", len(hits))
	}
	if hits[0].offset != 100 {
		t.Fatalf("hit offset = %d, want 100", hits[0].offset)
	}

	// after the skipped remainder, normal matching must resume cleanly
	hits, _ = a.feed(hits, []byte("resync\nfoo:HEAD:bar\n"), 2*maxLineBytes, proc, -1)
	hits, _ = a.flush(hits, proc, -1)
	if len(hits) != 2 || hits[1].line != "foo:HEAD:bar" {
		t.Fatalf("after resync: hits = %+v", hits)
	}
}

// runTxtCollectOnce streams one plain-text file through RunTxt and returns
// every hit (package-internal twin of the search_test harness helper).
func runTxtCollectOnce(t *testing.T, path string, pattern []byte) []Hit {
	t.Helper()
	hitCh := make(chan Hit, 64)
	err := RunTxt(TxtConfig{
		Ctx:        context.Background(),
		Pattern:    pattern,
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

// End-to-end (plain text): one ~6 MiB newline-free line containing exactly one
// HEAD occurrence must produce exactly one hit, not one per read window.
func TestRunTxtOversizedLineEmitsOnce(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.txt")

	// HEAD sits inside the first maxLineBytes so each re-emission of the
	// stale carry duplicates it; the line has no newline for ~6 MiB.
	line := bytes.Repeat([]byte("x"), maxLineBytes/2)
	line = append(line, bytes.Repeat([]byte("y"), maxLineBytes)...)
	copy(line[100:], []byte("HEAD"))
	body := append(line, []byte("\nplain HEAD tail\n")...)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}

	hits := runTxtCollectOnce(t, p, []byte("HEAD"))
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (one on the truncated huge line + tail)", len(hits))
	}
}

// End-to-end (zstd): same oversized-line shape inside one frame; the decode
// step boundaries cross maxLineBytes and must not duplicate the capped line.
func TestSearchZstOversizedLineEmitsOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.zst")

	line := bytes.Repeat([]byte("x"), maxLineBytes/2)
	line = append(line, bytes.Repeat([]byte("y"), maxLineBytes)...)
	copy(line[100:], []byte("HEAD"))
	body := append(line, []byte("\nplain HEAD tail\n")...)
	writeZSTFile(t, path, body)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hitCh := make(chan Hit, 64)
	err = Run(Config{
		Pattern:    []byte("HEAD"),
		Workers:    1,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		ArchiveOrd: map[string]int{path: 0},
		Hits:       hitCh,
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}
	var got []Hit
	for h := range hitCh {
		got = append(got, h)
	}
	if len(got) != 2 {
		t.Fatalf("hits = %d, want 2 (one on the truncated huge line + tail)", len(got))
	}
}
