package zstdframe_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"

	"github.com/klauspost/compress/zstd"
)

func writeZST(t *testing.T, path string, parts ...[]byte) {
	t.Helper()
	var buf bytes.Buffer
	for i, part := range parts {
		enc, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := enc.Write(part); err != nil {
			t.Fatal(err)
		}
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
		if i < len(parts)-1 {
			// separate frames by closing encoder between parts
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanFileSingleFrame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.zst")
	payload := []byte("alpha line\nbeta needle line\ngamma\n")
	writeZST(t, path, payload)

	frames, err := zstdframe.ScanFile(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].UncompressedEnd != int64(len(payload)) {
		t.Fatalf("uncompressed end = %d, want %d", frames[0].UncompressedEnd, len(payload))
	}
}

func TestScanFileMultiFrame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.zst")
	a := []byte("part-one\n")
	b := []byte("part-two needle\n")
	writeZST(t, path, a, b)

	frames, err := zstdframe.ScanFile(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[1].UncompressedStart != int64(len(a)) {
		t.Fatalf("second start = %d, want %d", frames[1].UncompressedStart, len(a))
	}
}
