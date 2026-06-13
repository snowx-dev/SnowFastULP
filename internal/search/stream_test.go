package search_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// A chunk larger than one decode-step is scanned in several reads, each flushed
// as it completes. Verify the per-step flush loses no hits at the read seams
// (a small DecodeStep forces many flushes over a single frame).
func TestSearchStreamsAcrossDecodeStepsNoLoss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.zst")
	const n = 100_000 // 12 B/line -> ~1.2 MB > outWin, spans many decode-steps
	writeRepeatedZST(t, path, "needle line\n", n)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan search.Hit, 256)
	var got int64
	done := make(chan struct{})
	go func() {
		for range hitCh {
			atomic.AddInt64(&got, 1)
		}
		close(done)
	}()

	err = search.Run(search.Config{
		Pattern:    []byte("needle"),
		Workers:    1,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		Hits:       hitCh,
		ArchiveOrd: map[string]int{path: 0},
		DecodeStep: 64 << 10, // small -> many decode-step flushes per chunk
	})
	close(hitCh)
	<-done

	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("hits = %d, want %d (per-step flush dropped hits at a seam?)", got, n)
	}
}
