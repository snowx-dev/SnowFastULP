package index_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
)

// corruptSidecar overwrites the library sidecar with invalid data.
func corruptSidecar(t *testing.T, archivePath string, data []byte) {
	t.Helper()
	if err := os.WriteFile(searchidx.LibrarySidecarPath(archivePath), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRebuildsCorruptSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeZST(t, path, []byte("hello world\n"))

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Garbage bytes: Load's JSON parse fails even though the sidecar is
	// non-stale (the archive was not touched after the build).
	corruptSidecar(t, path, []byte("not json at all"))
	sc2, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("ensure after corrupt sidecar: %v", err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta.Action)
	}
	if meta.Missing {
		t.Fatal("metadata must not claim missing for an existing sidecar")
	}
	if len(sc2.Chunks) != len(sc.Chunks) {
		t.Fatalf("rebuilt chunks = %d, want %d", len(sc2.Chunks), len(sc.Chunks))
	}
	// The rebuilt sidecar is valid again and reloads cleanly.
	sc3, meta3, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta3.Action != index.EnsureActionLoad {
		t.Fatalf("action after heal = %q, want load", meta3.Action)
	}
	if len(sc3.Chunks) != len(sc.Chunks) {
		t.Fatalf("reloaded chunks = %d", len(sc3.Chunks))
	}
}

func TestEnsureRebuildsFormatVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeZST(t, path, []byte("hello world\n"))

	if _, err := index.Build(context.Background(), path, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Well-formed JSON but failing format/version validation: Load must
	// reject it and Ensure must rebuild rather than propagate the failure.
	stale := index.Sidecar{Version: 9999, Format: "wrong.format", Chunks: []index.Chunk{{ChunkID: 0}}}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	corruptSidecar(t, path, data)

	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("ensure after version mismatch: %v", err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta.Action)
	}
	if len(sc.Chunks) != 1 {
		t.Fatalf("rebuilt chunks = %d, want 1", len(sc.Chunks))
	}
	if sc.Version != searchidx.FormatVersion || sc.Format != searchidx.FormatName {
		t.Fatalf("rebuilt sidecar format = %s v%d", sc.Format, sc.Version)
	}
}

func TestEnsureDictionaryFrameIsIndexed(t *testing.T) {
	// A real 2-byte dictionary ID frame must index end-to-end once the
	// matching dictionary is registered.
	dict := []byte("registered-dictionary-content-for-tests")
	payload := []byte("alpha line\nneedle beta\ngamma\n")
	zstdframe.RegisterDecoderDict(0x1234, dict)

	dir := t.TempDir()
	path := filepath.Join(dir, "dict.zst")
	writeDictArchive(t, path, 0x1234, dict, payload)

	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("ensure dict archive: %v", err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta.Action)
	}
	if len(sc.Chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(sc.Chunks))
	}
	if sc.Chunks[0].UncompressedEnd != int64(len(payload)) {
		t.Fatalf("uncompressed end = %d, want %d", sc.Chunks[0].UncompressedEnd, len(payload))
	}
}
