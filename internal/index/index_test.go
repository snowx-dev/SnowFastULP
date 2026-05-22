package index_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"

	"github.com/klauspost/compress/zstd"
)

func writeZST(t *testing.T, path string, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildLoadAndStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeZST(t, path, []byte("hello world\n"))

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Chunks) != 1 {
		t.Fatalf("chunks = %d", len(sc.Chunks))
	}

	loaded, err := index.Load(searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Source != "sample.zst" {
		t.Fatalf("source = %q", loaded.Source)
	}

	stale, err := index.IsStale(path, searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("expected fresh sidecar")
	}

	if err := os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	stale, err = index.IsStale(path, searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale sidecar after touching archive")
	}
}

func TestEnsureWritesToLibrarySubdir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeZST(t, path, []byte("hello world\n"))

	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta.Action)
	}
	if sc == nil || len(sc.Chunks) != 1 {
		t.Fatalf("unexpected sidecar: %+v", sc)
	}
	want := searchidx.LibrarySidecarPath(path)
	if meta.SidecarPath != want {
		t.Fatalf("sidecar path = %q, want %q", meta.SidecarPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("library sidecar missing at %s: %v", want, err)
	}
	if _, err := os.Stat(searchidx.LegacyAdjacentSidecarPath(path)); !os.IsNotExist(err) {
		t.Fatal("expected no adjacent sidecar when library subdir exists")
	}

	sc2, meta2, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta2.Action != index.EnsureActionLoad {
		t.Fatalf("second ensure action = %q, want load", meta2.Action)
	}
	if meta2.Stale {
		t.Fatal("expected fresh sidecar on reload")
	}
	if len(sc2.Chunks) != len(sc.Chunks) {
		t.Fatalf("reload chunks = %d, want %d", len(sc2.Chunks), len(sc.Chunks))
	}
}
