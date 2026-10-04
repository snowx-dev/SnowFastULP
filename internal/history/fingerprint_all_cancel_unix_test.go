//go:build unix

package history_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// Canceling mid-hash must stop the worker promptly: the context error comes
// back and, exactly like a single FingerprintFile, a read blocked in the
// kernel is unstuck by the fileabort registry (the WatchInterrupt path).
func TestFingerprintAllCancelMidHash(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "input.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	files := &fileabort.Registry{}
	ctx, cancel := context.WithCancel(fileabort.WithContext(context.Background(), files))
	defer cancel()

	sawProgress := make(chan struct{}, 1)
	progress := func(filesDone, filesTotal int, bytesDone, bytesTotal int64) {
		select {
		case sawProgress <- struct{}{}:
		default:
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := history.FingerprintAll(ctx,
			[]history.FingerprintUnit{{Path: fifo}}, 1, progress)
		done <- err
	}()

	// Rendezvous with the reader: this open blocks until the worker's
	// os.Open has connected the FIFO.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}
	<-sawProgress // worker is parked in a Read past the first chunk

	cancel()
	select {
	case err := <-done:
		t.Fatalf("FingerprintAll returned %v after plain cancel; a read blocked in the kernel needs the registry", err)
	case <-time.After(100 * time.Millisecond):
	}

	files.CloseAll()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FingerprintAll stayed blocked after cancel + CloseAll")
	}
}
