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

func TestScanFileCancelBeforeStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.zst")
	writeZST(t, path, []byte("hello\n"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := zstdframe.ScanFile(ctx, path, nil, nil)
	if err == nil {
		t.Fatal("expected cancel error")
	}
}

func TestScanFileCancelDuringDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.zst")
	payload := bytes.Repeat([]byte{'x'}, 128<<20)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// sync on first Progress callback, not time.Sleep. 5ms guess raced
	// under load and cancelled before first read. Progress = scan loop
	// is in flight and cancel will hit a real ctx.Err
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
