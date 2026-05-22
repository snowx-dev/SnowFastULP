package search_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"

	"github.com/klauspost/compress/zstd"
)

func TestSearchCancelDuringLargeChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.zst")
	payload := bytes.Repeat([]byte("x\n"), 8<<20)
	writeMultiFrameZST(t, path, payload)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// sync on metrics not sleep, BytesScanned > 0 = worker is in decode
	// loop and cancel will land at ctx.Err. 2s polling cap = loud fail
	ctx, cancel := context.WithCancel(context.Background())
	metrics := &search.Metrics{}
	hitCh := make(chan search.Hit, 8)
	done := make(chan error, 1)
	go func() {
		done <- search.Run(search.Config{
			Ctx:        ctx,
			Pattern:    []byte("missing-pattern"),
			Workers:    2,
			Archives:   []string{path},
			Sidecars:   map[string]*index.Sidecar{path: sc},
			Metrics:    metrics,
			Hits:       hitCh,
			ArchiveOrd: map[string]int{path: 0},
		})
		close(hitCh)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for metrics.BytesScanned.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("workers never reported scanned bytes")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("search did not stop after cancel")
	}
}

// pre-cancelled tasks must not fire OnArchiveDone, else OrderedPrinter
// flushes partial results as if archive were done
func TestRunCancelDoesNotMarkArchiveDone(t *testing.T) {
	dir := t.TempDir()
	archives := make([]string, 4)
	sidecars := map[string]*index.Sidecar{}
	ord := map[string]int{}
	for i := range archives {
		p := filepath.Join(dir, "a"+string(rune('0'+i))+".zst")
		writeSingleFrameZST(t, p, []byte("alpha\nbeta\n"))
		sc, err := index.Build(context.Background(), p, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		archives[i] = p
		sidecars[p] = sc
		ord[p] = i
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var doneCount atomic.Int32
	hitCh := make(chan search.Hit, 1)
	err := search.Run(search.Config{
		Ctx:           ctx,
		Pattern:       []byte("zzz"),
		Workers:       2,
		Archives:      archives,
		Sidecars:      sidecars,
		ArchiveOrd:    ord,
		Hits:          hitCh,
		OnArchiveDone: func(int) { doneCount.Add(1) },
	})
	close(hitCh)
	if err == nil {
		t.Fatal("expected context error")
	}
	if got := doneCount.Load(); got != 0 {
		t.Fatalf("OnArchiveDone fired %d times on cancel; expected 0", got)
	}
}

func writeSingleFrameZST(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
