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

// H-08: a chunk whose hits exactly fill the cap BEFORE the last decoded read
// must still be classified as capped whenever unconsumed chunk data remains.
// The pre-fix code returned "clean" the moment emitted == maxHits, so the
// remaining compressed data was never decoded: OnChunkCapped did not fire and
// the CLI reported exit 0 for incomplete results.
func TestSearchMaxHitsExactFillWithRemainingDataIsCapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exactfill.zst")

	// ten early hits fill the cap within the first decode steps; 300 KiB of
	// non-matching data follows, then an eleventh needle proving the chunk had
	// more to scan.
	var body []byte
	for i := 0; i < 10; i++ {
		body = append(body, []byte("needle line\n")...)
	}
	body = append(body, bytes.Repeat([]byte("filler filler\n"), 300*1024/14)...)
	body = append(body, []byte("needle eleven\n")...)
	writeBytesZST(t, path, body)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan search.Hit, 64)
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
		DecodeStep:      4096,
		Workers:         1,
		Archives:        []string{path},
		Sidecars:        map[string]*index.Sidecar{path: sc},
		Hits:            hitCh,
		ArchiveOrd:      map[string]int{path: 0},
		MaxHitsPerChunk: 10,
		OnChunkCapped: func(archive string, chunkID, emitted int) {
			atomic.AddInt32(&capEvents, 1)
		},
	})
	close(hitCh)
	<-done
	if err != nil {
		t.Fatal(err)
	}

	if got != 10 {
		t.Fatalf("hits = %d, want 10 (cap)", got)
	}
	if capEvents != 1 {
		t.Fatalf("OnChunkCapped fired %d times; want 1 (scan stopped before chunk end)", capEvents)
	}
}
