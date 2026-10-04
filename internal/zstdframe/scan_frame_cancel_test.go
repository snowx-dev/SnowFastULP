package zstdframe_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"

	"github.com/klauspost/compress/zstd"
)

// synthetic version, prior one needed a personal /run/media/... path
// and skipped in CI = dead code

func TestScanFileCancelDuringFrameScanSynthetic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "many-blocks.zst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{'a'}, 16<<20)
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// sync on first Progress, see scan_cancel_test.go rationale
	started := make(chan struct{}, 1)
	prog := func(_, _ int64) {
		select {
		case started <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := zstdframe.ScanFile(ctx, path, prog, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("ScanFile never reported initial progress")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ScanFile did not stop after cancel")
	}
}
