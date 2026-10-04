package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
)

// stubDecoder delivers its data once with a terminal (non-EOF) error on the
// SAME read — the "n>0 plus error" iteration the cap/error regression pins.
type stubDecoder struct {
	data []byte
	err  error
}

func (s *stubDecoder) Reset(_ io.Reader) error { return nil }

func (s *stubDecoder) Read(p []byte) (int, error) {
	n := copy(p, s.data)
	s.data = s.data[n:]
	return n, s.err
}

// TestSearchChunkCapWithDecoderError: when dec.Read returns bytes AND a
// non-EOF error on the same iteration that reaches the hit cap, searchChunk
// must flush the hits (emitting them) and return the decoder error together
// with the cap state. The pre-fix code returned nil and swallowed the error.
func TestSearchChunkCapWithDecoderError(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "chunk.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	decErr := errors.New("decoder blew up mid-stream")
	// 5 needle lines in one read; cap is 2 → flush truncates to 2 and flags
	// capped, then the same-iteration error must surface.
	var data []byte
	for i := 0; i < 5; i++ {
		data = append(data, []byte("needle line\n")...)
	}
	dec := &stubDecoder{data: data, err: decErr}

	var emittedLines []string
	emit := func(batch []localHit) error {
		for i := range batch {
			emittedLines = append(emittedLines, batch[i].line)
		}
		return nil
	}

	emitted, capped, _, err := searchChunk(
		context.Background(), f, dec,
		index.Chunk{ChunkID: 0, CompressedOffset: 0, CompressedSize: 1,
			UncompressedStart: 0, UncompressedEnd: int64(len(data))},
		[]byte("needle"), false, nil, nil, nil,
		make([]byte, outWin), outWin, 2, emit, lineAsmState{}, true)

	if err == nil {
		t.Fatal("decoder error swallowed: want the mid-stream error returned")
	}
	if !errors.Is(err, decErr) {
		t.Fatalf("err = %v, want %v", err, decErr)
	}
	if !capped {
		t.Fatal("capped = false, want true (hits exceeded the cap)")
	}
	if emitted != 2 {
		t.Fatalf("emitted = %d, want 2 (cap)", emitted)
	}
	if len(emittedLines) != 2 {
		t.Fatalf("flushed %d hits, want 2", len(emittedLines))
	}
}

// TestSearchChunkDecoderErrorWithoutCap: the same n>0 + non-EOF error without
// any cap must also surface the error (hits already flushed).
func TestSearchChunkDecoderErrorWithoutCap(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "chunk.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	decErr := errors.New("decoder blew up")
	var data []byte
	for i := 0; i < 5; i++ {
		data = append(data, []byte("needle line\n")...)
	}
	dec := &stubDecoder{data: data, err: decErr}

	emitted, capped, _, err := searchChunk(
		context.Background(), f, dec,
		index.Chunk{ChunkID: 0, UncompressedStart: 0, UncompressedEnd: int64(len(data))},
		[]byte("needle"), false, nil, nil, nil,
		make([]byte, outWin), outWin, 0,
		func(batch []localHit) error { return nil }, lineAsmState{}, true)

	if !errors.Is(err, decErr) {
		t.Fatalf("err = %v, want %v", err, decErr)
	}
	if capped {
		t.Fatal("capped = true with no cap configured")
	}
	if emitted != 5 {
		t.Fatalf("emitted = %d, want 5", emitted)
	}
}

// TestRunCollectsChunkErrorsIntoPartialScanError: a chunk whose archive cannot
// be opened (missing file) must keep per-chunk continuation, still fire
// OnChunkError, and surface at the end as a *PartialScanError naming the
// archive and chunk — never a silent exit-0 scan.
func TestRunCollectsChunkErrorsIntoPartialScanError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.zst")

	sc := &index.Sidecar{Chunks: []index.Chunk{{
		ChunkID: 3, CompressedOffset: 0, CompressedSize: 16,
		UncompressedStart: 0, UncompressedEnd: 64,
	}}}
	var callbackErrs int
	err := Run(Config{
		Ctx:        context.Background(),
		Pattern:    []byte("needle"),
		Workers:    1,
		Archives:   []string{missing},
		Sidecars:   map[string]*index.Sidecar{missing: sc},
		Hits:       make(chan Hit, 8),
		ArchiveOrd: map[string]int{missing: 0},
		OnChunkError: func(archive string, chunkID int, err error) {
			callbackErrs++
		},
	})
	if err == nil {
		t.Fatal("Run swallowed the open failure: want PartialScanError")
	}
	var perr *PartialScanError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *PartialScanError", err, err)
	}
	if callbackErrs == 0 {
		t.Fatal("OnChunkError callback never fired")
	}
	if len(perr.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(perr.Failures))
	}
	if f := perr.Failures[0]; f.Path != missing || f.ChunkID != 3 {
		t.Fatalf("failure = %+v, want path=%s chunk=3", f, missing)
	}
}

// TestRunTxtCollectsFileErrorsIntoPartialScanError: one unreadable txt file
// must not hide hits from a readable one, and the unreadable file must be
// named by the returned PartialScanError.
func TestRunTxtCollectsFileErrorsIntoPartialScanError(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	if err := os.WriteFile(good, []byte("needle line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.txt")

	hits := make(chan Hit, 8)
	err := RunTxt(TxtConfig{
		Ctx:        context.Background(),
		Pattern:    []byte("needle"),
		Workers:    2,
		Files:      []string{good, missing},
		Hits:       hits,
		ArchiveOrd: map[string]int{good: 0, missing: 1},
	})
	if err == nil {
		t.Fatal("RunTxt swallowed the open failure: want PartialScanError")
	}
	var perr *PartialScanError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *PartialScanError", err, err)
	}
	if len(perr.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(perr.Failures))
	}
	if perr.Failures[0].Path != missing {
		t.Fatalf("failure path = %s, want %s", perr.Failures[0].Path, missing)
	}
	close(hits)
	var got []string
	for h := range hits {
		got = append(got, h.Line)
	}
	if len(got) != 1 || !strings.Contains(got[0], "needle") {
		t.Fatalf("hits from the good file = %v, want the needle line", got)
	}
}

// TestPartialScanErrorMessageSortedCappedExactCount: the message is
// deterministic (sorted by path then chunk), lists at most 10 examples, and
// always preserves the exact failure count.
func TestPartialScanErrorMessageSortedCappedExactCount(t *testing.T) {
	var fs []PartialFailure
	for i := 0; i < 12; i++ {
		fs = append(fs, PartialFailure{
			Path:    fmt.Sprintf("/d/f%02d.zst", i%3),
			ChunkID: i % 2,
			Err:     fmt.Errorf("boom %d", i),
		})
	}
	msg := (&PartialScanError{Failures: fs}).Error()

	if !strings.Contains(msg, "12") {
		t.Fatalf("message must preserve the exact count 12:\n%s", msg)
	}
	if !strings.Contains(msg, "showing 10") {
		t.Fatalf("message must say it shows 10 of 12:\n%s", msg)
	}
	if n := strings.Count(msg, "\n  /d/"); n != 10 {
		t.Fatalf("example lines = %d, want 10:\n%s", n, msg)
	}

	// deterministic order: sorted by path, then chunk (golden message)
	golden := (&PartialScanError{Failures: []PartialFailure{
		{Path: "/b.zst", ChunkID: 0, Err: errors.New("x")},
		{Path: "/a.zst", ChunkID: 5, Err: errors.New("y")},
		{Path: "/a.zst", ChunkID: 2, Err: errors.New("z")},
	}}).Error()
	want := "incomplete scan: 3 failure(s):\n" +
		"  /a.zst: chunk 2: z\n" +
		"  /a.zst: chunk 5: y\n" +
		"  /b.zst: chunk 0: x"
	if golden != want {
		t.Fatalf("message not sorted by path then chunk:\ngot:\n%s\nwant:\n%s", golden, want)
	}

	// small case: every failure listed, no truncation note
	small := (&PartialScanError{Failures: []PartialFailure{
		{Path: "/a.zst", ChunkID: 2, Err: errors.New("x")},
		{Path: "/b.txt", ChunkID: -1, Err: errors.New("y")},
	}}).Error()
	if strings.Contains(small, "showing") {
		t.Fatalf("small case must list everything without a showing note:\n%s", small)
	}
	if !strings.Contains(small, "/a.zst") || !strings.Contains(small, "chunk 2") {
		t.Fatalf("small case missing chunk example:\n%s", small)
	}
	if !strings.Contains(small, "/b.txt") || strings.Contains(small, "chunk -1") {
		t.Fatalf("non-chunk failure must omit the chunk part:\n%s", small)
	}
}
