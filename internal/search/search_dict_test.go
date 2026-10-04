package search_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"

	"github.com/klauspost/compress/zstd"
)

// TestSearchDictionaryArchive: a real 2-byte dictionary-ID archive must be
// indexed AND searched end-to-end. Both the frame scan (index side) and the
// search workers need the matching dictionary registered; without it, zstd
// decoders reject the frame outright ("unknown dictionary").
func TestSearchDictionaryArchive(t *testing.T) {
	dict := []byte("search-dictionary-content-for-tests")
	payload := []byte("alpha line\nneedle one\nbeta line\nneedle two\n")
	zstdframe.RegisterDecoderDict(0x1234, dict)

	dir := t.TempDir()
	path := filepath.Join(dir, "dict.zst")
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderDictRaw(0x1234, dict))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("index dict archive: %v", err)
	}

	hitCh := make(chan search.Hit, 16)
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
	})
	close(hitCh)
	<-done

	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
}
