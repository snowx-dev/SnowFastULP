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

func TestScanFileReportsFrameScanProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.zst")

	payload := bytes.Repeat([]byte("needle line with some padding\n"), 512*1024)
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
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

	var maxDone int64
	prog := func(done, total int64) {
		if done > maxDone {
			maxDone = done
		}
		if total <= 0 {
			t.Fatal("expected total > 0")
		}
	}

	frames, err := zstdframe.ScanFile(context.Background(), path, prog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if maxDone <= 0 {
		t.Fatalf("expected frame-scan progress callbacks, maxDone=%d", maxDone)
	}
}

func TestScanFileActivityHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.zst")
	payload := []byte("hello\n")
	writeZST(t, path, payload)

	var frameScan, decode int
	act := &zstdframe.Activity{
		FrameScan: func(start bool) {
			if start {
				frameScan++
			}
		},
		Decode: func(start bool) {
			if start {
				decode++
			}
		},
	}

	if _, err := zstdframe.ScanFile(context.Background(), path, nil, act); err != nil {
		t.Fatal(err)
	}
	if frameScan != 1 || decode != 1 {
		t.Fatalf("frameScan=%d decode=%d, want 1 each", frameScan, decode)
	}
}
