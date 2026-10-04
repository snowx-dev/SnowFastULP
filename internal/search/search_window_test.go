package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/snowx-dev/SnowFastULP/internal/index"
)

// craftedLongWindowFrame builds a minimal valid zstd frame whose header
// declares a 256 MiB window (window descriptor 0x90 → windowLog 28): magic +
// FHD 0x00 + window descriptor + one raw last block with the payload.
func craftedLongWindowFrame(payload []byte) []byte {
	f := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x90}
	hdr := uint32(len(payload))<<3 | 1 // last block, raw, size n
	f = append(f, byte(hdr), byte(hdr>>8), byte(hdr>>16))
	return append(f, payload...)
}

// TestRunPartialScanNamesLongWindowFix: a chunk whose frame declares a window
// above the 128 MiB decoder cap must surface through the partial-scan
// collector as a user-facing failure naming the cap and the re-compression
// remediation — never a debug-only note.
func TestRunPartialScanNamesLongWindowFix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "longwin.zst")
	frame := craftedLongWindowFrame([]byte("hello"))
	if err := os.WriteFile(path, frame, 0o644); err != nil {
		t.Fatal(err)
	}

	sc := &index.Sidecar{Chunks: []index.Chunk{{
		ChunkID:           0,
		CompressedOffset:  0,
		CompressedSize:    int64(len(frame)),
		UncompressedStart: 0,
		UncompressedEnd:   int64(len([]byte("hello"))),
	}}}
	err := Run(Config{
		Ctx:        context.Background(),
		Pattern:    []byte("needle"),
		Workers:    1,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		Hits:       make(chan Hit, 8),
		ArchiveOrd: map[string]int{path: 0},
	})
	if err == nil {
		t.Fatal("long-window chunk must fail the 128 MiB cap")
	}
	var perr *PartialScanError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %T (%v), want *PartialScanError (incomplete scan, not a debug note)", err, err)
	}
	msg := perr.Error()
	for _, want := range []string{"128 MiB", "--long", "longwin.zst"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("partial-scan message missing %q:\n%s", want, msg)
		}
	}
	// the underlying sentinel must still unwrap
	if !errors.Is(err, zstd.ErrWindowSizeExceeded) {
		t.Fatalf("partial error must wrap zstd.ErrWindowSizeExceeded, got: %v", err)
	}
}
