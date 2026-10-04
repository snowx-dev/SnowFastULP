package search_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// writeConcatFramesZST writes one .zst archive made of len(frames) INDEPENDENT
// zstd frames concatenated back to back — the shape a mid-line frame split
// produces (frame 1 ends without a newline, frame 2 completes the line).
func writeConcatFramesZST(t *testing.T, path string, frames ...[]byte) {
	t.Helper()
	var buf bytes.Buffer
	for _, body := range frames {
		enc, err := zstd.NewWriter(&buf)
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
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runArchiveSearch searches one indexed archive and returns every emitted hit.
func runArchiveSearch(t *testing.T, path string, sc *index.Sidecar, pattern []byte, workers int) []search.Hit {
	t.Helper()
	hitCh := make(chan search.Hit, 64)
	var hits []search.Hit
	done := make(chan struct{})
	go func() {
		for h := range hitCh {
			hits = append(hits, h)
		}
		close(done)
	}()
	err := search.Run(search.Config{
		Pattern:    pattern,
		Workers:    workers,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		Hits:       hitCh,
		ArchiveOrd: map[string]int{path: 0},
	})
	close(hitCh)
	<-done
	if err != nil {
		t.Fatalf("search.Run: %v", err)
	}
	return hits
}

// The H-10 review proof, byte for byte: a .zst of two concatenated frames,
// frame 1 = "alpha NEED", frame 2 = "LE beta\n". `zstd -dc` yields
// "alpha NEEDLE beta\n". index.Build gives one chunk per frame, so the line
// straddles a CHUNK boundary. Pre-fix, each chunk assembled lines
// independently: "NEEDLE" found nothing and "NEED" emitted the truncated
// fragment "alpha NEED".
func TestSearchCarriesLineAcrossConcatenatedFrames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "split.zst")
	writeConcatFramesZST(t, path, []byte("alpha NEED"), []byte("LE beta\n"))

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Chunks) != 2 {
		t.Fatalf("fixture precondition: got %d indexed chunks, want 2 (one per frame)", len(sc.Chunks))
	}

	// The joined line must match across the frame/chunk boundary.
	for _, workers := range []int{1, 4} {
		hits := runArchiveSearch(t, path, sc, []byte("NEEDLE"), workers)
		if len(hits) != 1 {
			t.Fatalf("workers=%d: NEEDLE hits = %d (%v), want 1", workers, len(hits), hits)
		}
		if hits[0].Line != "alpha NEEDLE beta" {
			t.Fatalf("workers=%d: NEEDLE line = %q, want %q", workers, hits[0].Line, "alpha NEEDLE beta")
		}
		if hits[0].Offset != 6 {
			t.Fatalf("workers=%d: NEEDLE offset = %d, want 6 (match position inside the joined line)", workers, hits[0].Offset)
		}
		if hits[0].ChunkID != 1 {
			t.Fatalf("workers=%d: NEEDLE hit emitted from chunk %d, want 1 (the completing chunk)", workers, hits[0].ChunkID)
		}

		// The shorter pattern must NOT surface the pre-fix truncated
		// fragment "alpha NEED" as its own line.
		hits = runArchiveSearch(t, path, sc, []byte("NEED"), workers)
		if len(hits) != 1 || hits[0].Line != "alpha NEEDLE beta" {
			t.Fatalf("workers=%d: NEED hits = %v, want exactly [%q]", workers, hits, "alpha NEEDLE beta")
		}
	}
}

// The handoff is per-archive, never global: several archives' chunks may be
// picked up by different workers in arbitrary order, and each archive's own
// chunk sequence must still be stitched mid-line. A single-chunk archive
// must be unaffected (its trailing partial line still flushes at EOF).
func TestRunPerArchiveHandoffAcrossInterleavedWorkers(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "a.zst"),
		filepath.Join(dir, "b.zst"),
		filepath.Join(dir, "solo.zst"),
	}
	writeConcatFramesZST(t, paths[0], []byte("one NEED"), []byte("LE alpha\n"))
	writeConcatFramesZST(t, paths[1], []byte("two NEED"), []byte("LE beta\n"))
	writeConcatFramesZST(t, paths[2], []byte("solo NEEDLE gamma\n"))

	sidecars := make(map[string]*index.Sidecar, len(paths))
	ord := make(map[string]int, len(paths))
	for i, p := range paths {
		sc, err := index.Build(context.Background(), p, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sidecars[p] = sc
		ord[p] = i
	}
	if len(sidecars[paths[0]].Chunks) != 2 || len(sidecars[paths[1]].Chunks) != 2 ||
		len(sidecars[paths[2]].Chunks) != 1 {
		t.Fatalf("fixture precondition: chunks = %d/%d/%d, want 2/2/1",
			len(sidecars[paths[0]].Chunks), len(sidecars[paths[1]].Chunks), len(sidecars[paths[2]].Chunks))
	}

	hitCh := make(chan search.Hit, 64)
	var hits []search.Hit
	done := make(chan struct{})
	go func() {
		for h := range hitCh {
			hits = append(hits, h)
		}
		close(done)
	}()
	err := search.Run(search.Config{
		Pattern:    []byte("NEEDLE"),
		Workers:    3, // more workers than needed → chunk dispatch genuinely interleaves
		Archives:   paths,
		Sidecars:   sidecars,
		Hits:       hitCh,
		ArchiveOrd: ord,
	})
	close(hitCh)
	<-done
	if err != nil {
		t.Fatalf("search.Run: %v", err)
	}

	want := map[string]int{
		"one NEEDLE alpha":  0, // placeholder set below
		"two NEEDLE beta":   0,
		"solo NEEDLE gamma": 0,
	}
	got := make(map[string]int, len(hits))
	for _, h := range hits {
		got[h.Line]++
	}
	want = map[string]int{
		"one NEEDLE alpha":  1,
		"two NEEDLE beta":   1,
		"solo NEEDLE gamma": 1,
	}
	if len(got) != len(want) {
		t.Fatalf("got lines %v (hits=%d), want exactly %v", got, len(hits), want)
	}
	for line, n := range want {
		if got[line] != n {
			t.Fatalf("line %q seen %d times, want %d (all hits: %v)", line, got[line], n, hits)
		}
	}
}
