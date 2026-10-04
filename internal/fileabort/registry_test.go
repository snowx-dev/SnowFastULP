package fileabort_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

func TestRegistryCloseAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.zst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	reg := &fileabort.Registry{}
	unreg := reg.Register(f)
	reg.CloseAll()
	unreg()

	if _, err := f.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected read on closed file to fail")
	}
}

// TestRegistryRegisterAfterCloseAllClosesImmediately: after the abort sweep
// begins, no handle may escape it. A file registered after CloseAll must be
// closed immediately by Register, its unregister must be a safe no-op, and
// repeated CloseAll must stay safe.
func TestRegistryRegisterAfterCloseAllClosesImmediately(t *testing.T) {
	dir := t.TempDir()
	swept, err := os.Create(filepath.Join(dir, "swept"))
	if err != nil {
		t.Fatal(err)
	}
	reg := &fileabort.Registry{}
	reg.Register(swept)
	reg.CloseAll()

	late, err := os.Create(filepath.Join(dir, "late"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := late.WriteString("escape"); err != nil {
		t.Fatal(err)
	}
	unreg := reg.Register(late)

	// the late handle must already be closed: a Seek or Read must fail
	if _, err := late.Seek(0, io.SeekStart); err == nil {
		if n, rerr := late.Read(make([]byte, 6)); rerr == nil && n == 6 {
			t.Fatal("handle registered after CloseAll escaped the abort sweep (still readable)")
		}
	}
	unreg() // must be a safe no-op, never panic

	// repeated CloseAll must remain safe
	reg.CloseAll()
	reg.CloseAll()
}
