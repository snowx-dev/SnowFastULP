package searchidx_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

func TestWriteSidecarPathGolden(t *testing.T) {
	arch := "/data/library/sfu_part.txt.zst"
	want := "/data/library/sfu_search_idx/sfu_part.txt.zst.sfsidx.json"
	if got := searchidx.WriteSidecarPath(arch); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveExistingSidecarPrefersLibrary(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	if err := os.WriteFile(arch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := searchidx.LibrarySidecarPath(arch)
	adj := searchidx.LegacyAdjacentSidecarPath(arch)
	for _, p := range []string{lib, adj} {
		if err := os.MkdirAll(filepath.Dir(lib), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := searchidx.ResolveExistingSidecar(arch)
	if !ok || got != lib {
		t.Fatalf("ResolveExistingSidecar = (%q, %v), want (%q, true)", got, ok, lib)
	}
}
