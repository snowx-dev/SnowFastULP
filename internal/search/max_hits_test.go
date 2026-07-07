package search_test

import (
	"bytes"
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// writeRepeatedZST writes count copies of line into a single-frame zst at path,
// delegating the frame plumbing to writeBytesZST.
func writeRepeatedZST(t *testing.T, path string, line string, count int) {
	t.Helper()
	writeBytesZST(t, path, bytes.Repeat([]byte(line), count))
}

func TestSearchMaxHitsPerChunkTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.zst")
	// 10000 needle lines = 10000 matches uncapped
	writeRepeatedZST(t, path, "needle line\n", 10000)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan search.Hit, 100)
	var got int64
	done := make(chan struct{})
	go func() {
		for range hitCh {
			atomic.AddInt64(&got, 1)
		}
		close(done)
	}()

	var capEvents int32
	var cappedHits int
	err = search.Run(search.Config{
		Pattern:         []byte("needle"),
		Workers:         1,
		Archives:        []string{path},
		Sidecars:        map[string]*index.Sidecar{path: sc},
		Hits:            hitCh,
		ArchiveOrd:      map[string]int{path: 0},
		MaxHitsPerChunk: 50,
		OnChunkCapped: func(archive string, chunkID, emitted int) {
			atomic.AddInt32(&capEvents, 1)
			cappedHits = emitted
		},
	})
	close(hitCh)
	<-done

	if err != nil {
		t.Fatal(err)
	}
	if got != 50 {
		t.Fatalf("hits = %d, want exactly 50 (cap)", got)
	}
	if capEvents != 1 {
		t.Fatalf("OnChunkCapped fired %d times, want exactly 1", capEvents)
	}
	if cappedHits != 50 {
		t.Fatalf("OnChunkCapped emitted=%d, want 50", cappedHits)
	}
}

func TestSearchMaxHitsZeroIsUnbounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many.zst")
	writeRepeatedZST(t, path, "needle line\n", 500)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan search.Hit, 100)
	var got int64
	done := make(chan struct{})
	go func() {
		for range hitCh {
			atomic.AddInt64(&got, 1)
		}
		close(done)
	}()

	var capEvents int32
	err = search.Run(search.Config{
		Pattern:    []byte("needle"),
		Workers:    1,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		Hits:       hitCh,
		ArchiveOrd: map[string]int{path: 0},
		// MaxHitsPerChunk left at 0
		OnChunkCapped: func(string, int, int) {
			atomic.AddInt32(&capEvents, 1)
		},
	})
	close(hitCh)
	<-done

	if err != nil {
		t.Fatal(err)
	}
	if got != 500 {
		t.Fatalf("hits = %d, want 500 (no cap)", got)
	}
	if capEvents != 0 {
		t.Fatalf("OnChunkCapped fired %d times with cap=0; want 0", capEvents)
	}
}

func TestSearchMaxHitsExactBoundary(t *testing.T) {
	// hits == cap should not fire OnChunkCapped, off-by-one regression
	// where >=cap fires even when exactly cap hits exhausted the chunk
	dir := t.TempDir()
	path := filepath.Join(dir, "exact.zst")
	writeRepeatedZST(t, path, "needle line\n", 50)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan search.Hit, 100)
	var got int64
	done := make(chan struct{})
	go func() {
		for range hitCh {
			atomic.AddInt64(&got, 1)
		}
		close(done)
	}()

	var capEvents int32
	err = search.Run(search.Config{
		Pattern:         []byte("needle"),
		Workers:         1,
		Archives:        []string{path},
		Sidecars:        map[string]*index.Sidecar{path: sc},
		Hits:            hitCh,
		ArchiveOrd:      map[string]int{path: 0},
		MaxHitsPerChunk: 50,
		OnChunkCapped: func(string, int, int) {
			atomic.AddInt32(&capEvents, 1)
		},
	})
	close(hitCh)
	<-done

	if err != nil {
		t.Fatal(err)
	}
	if got != 50 {
		t.Fatalf("hits = %d, want 50", got)
	}
	// Path B: hits == cap with no overflow is not truncation, so OnChunkCapped
	// must not fire at the exact boundary.
	if capEvents != 0 {
		t.Errorf("OnChunkCapped fired %d time(s) at the exact boundary; want 0 (no truncation)", capEvents)
	}
}
