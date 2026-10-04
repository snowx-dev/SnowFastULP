//go:build unix

package history_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

func TestOpenRejectsNonRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := history.Open(path)
	if err == nil {
		t.Fatal("Open() against a FIFO succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Open() error = %q, want it to contain \"not a regular file\"", err)
	}
	_ = os.Remove(path)
}

func TestOpenReadOnlyRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := history.OpenReadOnly(path)
	if err == nil {
		t.Fatal("OpenReadOnly() against a FIFO succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("OpenReadOnly() error = %q, want it to contain \"not a regular file\"", err)
	}
}
