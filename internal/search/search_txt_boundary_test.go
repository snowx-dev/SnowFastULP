package search_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// pattern longer than file, findHits returns nothing, no panic on empty buf
func TestRunTxtFileSmallerThanOverlap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tiny.txt")
	if err := os.WriteFile(p, []byte("ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtCollect(t, p, []byte("looooong-pattern"))
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits, got %v", hits)
	}
}

func TestRunTxtEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtCollect(t, p, []byte("needle"))
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits, got %v", hits)
	}
}

// pattern straddles 1-MiB read boundary, overlap window must find it once
func TestRunTxtPatternStraddlingReadBoundary(t *testing.T) {
	const step = 1 << 20 // mirrors search.defaultDecodeStep
	pattern := []byte("STRADDLE")
	dir := t.TempDir()
	p := filepath.Join(dir, "straddle.txt")
	// <step - patLen/2 'a'>STRADDLE<padding>
	pre := step - len(pattern)/2
	body := bytes.Repeat([]byte("a"), pre)
	body = append(body, pattern...)
	body = append(body, bytes.Repeat([]byte("b"), 100)...)
	body = append(body, '\n')
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtCollect(t, p, pattern)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if !strings.Contains(hits[0].Line, "STRADDLE") {
		t.Fatalf("hit line = %q", hits[0].Line)
	}
}

// line spans 2 read iterations, pattern lands in iter 2. on-disk backref
// must recover the whole line, kept under 64 KiB backref budget
func TestRunTxtMatchAtChunkSeam(t *testing.T) {
	const step = 1 << 20 // mirrors search.defaultDecodeStep
	pattern := []byte("SEAM-NEEDLE")
	dir := t.TempDir()
	p := filepath.Join(dir, "seam.txt")
	// <226 KiB 'a'><lf><30 KiB 'b'><pattern><" tail\n">
	// pattern lands at ~256 KiB+1, ie iter 2
	headFiller := step - 30*1024 // 226 KiB
	body := bytes.Repeat([]byte("a"), headFiller)
	body = append(body, '\n')
	body = append(body, bytes.Repeat([]byte("b"), 30*1024)...)
	body = append(body, pattern...)
	body = append(body, []byte(" tail of line\n")...)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtCollect(t, p, pattern)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	got := hits[0].Line
	// line starts w/ 30 KiB 'b' right after the lf
	if !strings.HasPrefix(got, "bbb") || !strings.Contains(got, "tail of line") {
		t.Fatalf("line truncated, first 40 %q ... last 40 %q",
			got[:min(40, len(got))], got[max(0, len(got)-40):])
	}
	if strings.HasPrefix(got, "SEAM") {
		t.Fatalf("line starts w/ pattern, backref didnt run")
	}
}

func TestRunTxtNoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "no-nl.txt")
	if err := os.WriteFile(p, []byte("hit-line-no-newline-needle"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits := runTxtCollect(t, p, []byte("needle"))
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].Line != "hit-line-no-newline-needle" {
		t.Fatalf("line = %q", hits[0].Line)
	}
}

func TestRunTxtManyFilesWorkerPool(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, 32)
	ord := map[string]int{}
	for i := range files {
		p := filepath.Join(dir, "f"+itoa(i)+".txt")
		if err := os.WriteFile(p, []byte("alpha needle beta\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		files[i] = p
		ord[p] = i
	}
	hitCh := make(chan search.Hit, 64)
	metrics := &search.Metrics{}
	err := search.RunTxt(search.TxtConfig{
		Ctx:        context.Background(),
		Pattern:    []byte("needle"),
		Workers:    4,
		Files:      files,
		ArchiveOrd: ord,
		Metrics:    metrics,
		Hits:       hitCh,
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for range hitCh {
		count++
	}
	if count != 32 {
		t.Fatalf("hits = %d, want 32", count)
	}
	if got := metrics.ChunksDone.Load(); got != 32 {
		t.Fatalf("ChunksDone = %d, want 32", got)
	}
}

func TestRunTxtCancelDoesNotFireOnFileDone(t *testing.T) {
	dir := t.TempDir()
	files := make([]string, 8)
	ord := map[string]int{}
	for i := range files {
		p := filepath.Join(dir, "f"+itoa(i)+".txt")
		if err := os.WriteFile(p, []byte("data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		files[i] = p
		ord[p] = i
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var doneCount atomic.Int32
	hitCh := make(chan search.Hit, 1)
	err := search.RunTxt(search.TxtConfig{
		Ctx:        ctx,
		Pattern:    []byte("zzz"),
		Workers:    2,
		Files:      files,
		ArchiveOrd: ord,
		Hits:       hitCh,
		OnFileDone: func(int) { doneCount.Add(1) },
	})
	close(hitCh)
	if err == nil {
		t.Fatal("expected context error")
	}
	if got := doneCount.Load(); got != 0 {
		t.Fatalf("OnFileDone fired %d times on cancel; expected 0", got)
	}
}

func runTxtCollect(t *testing.T, path string, pattern []byte) []search.Hit {
	t.Helper()
	hitCh := make(chan search.Hit, 64)
	err := search.RunTxt(search.TxtConfig{
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
	var hits []search.Hit
	for h := range hitCh {
		hits = append(hits, h)
	}
	return hits
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
