package fileabort_test

import (
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
