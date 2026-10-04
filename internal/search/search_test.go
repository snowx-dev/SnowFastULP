package search_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"

	"github.com/klauspost/compress/zstd"
)

func writeMultiFrameZST(t *testing.T, path string, parts ...[]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, part := range parts {
		enc, err := zstd.NewWriter(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Write(part); err != nil {
			t.Fatal(err)
		}
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchOverlapAcrossWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlap.zst")

	const win = 1 << 20
	payload := bytes.Repeat([]byte("x"), win+64)
	needle := []byte("NEEDLE")
	copy(payload[win-3:], needle)

	writeMultiFrameZST(t, path, payload)

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan search.Hit, 8)
	metrics := &search.Metrics{}
	err = search.Run(search.Config{
		Pattern:    needle,
		Workers:    2,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		Metrics:    metrics,
		Hits:       hitCh,
		ArchiveOrd: map[string]int{path: 0},
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}
	var hits []search.Hit
	for h := range hitCh {
		hits = append(hits, h)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if total := metrics.BytesScannedTotal.Load(); total != int64(len(payload)) {
		t.Fatalf("BytesScannedTotal = %d, want %d", total, len(payload))
	}
}
