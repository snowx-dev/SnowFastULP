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

// A fingerprint read blocked on an empty FIFO must be unstuck when the
// context's fileabort registry closes its handles (the WatchInterrupt path on
// Ctrl-C): the blocked Read returns and FingerprintFile reports the error
// promptly instead of hanging until the writer closes.
func TestFingerprintFileAbortClosesBlockedRead(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "input.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	files := &fileabort.Registry{}
	ctx := fileabort.WithContext(context.Background(), files)
	done := make(chan error, 1)
	go func() {
		// The read-side open blocks until the writer below opens; the read
		// itself then blocks forever after the first line (no EOF).
		_, err := history.FingerprintFile(ctx, fifo, nil)
		done <- err
	}()
	// Rendezvous with the reader: this open blocks until FingerprintFile's
	// os.Open has connected the FIFO.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		t.Fatalf("fingerprint returned before any abort: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	files.CloseAll()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked fingerprint succeeded after CloseAll; want a read error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fingerprint stayed blocked after CloseAll closed the registered handle")
	}
}
