package index_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

// M-08: a structurally incomplete chunk map (dropped frame, overlapping or
// non-contiguous coverage) must not be accepted as fresh — the missing frames
// would silently produce false negatives. Load must reject it and Ensure must
// self-heal by rebuilding, exactly as it does for corrupt/unparseable
// sidecars.

// twoFrameArchive writes a two-frame zstd archive (two concatenated streams)
// and returns its path.
func twoFrameArchive(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "sample.zst")
	data := append(zstdBytes(t, []byte("alpha line one\nneedle original\n")),
		zstdBytes(t, []byte("beta line two\nneedle second\n"))...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadSidecar reads the sidecar JSON from disk.
func loadSidecar(t *testing.T, path string) index.Sidecar {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sc index.Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

func writeSidecar(t *testing.T, path string, sc index.Sidecar) {
	t.Helper()
	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// sidecarPath resolves where Ensure wrote the sidecar for the archive.
func sidecarPath(t *testing.T, archivePath string) string {
	t.Helper()
	p, ok := searchidx.ResolveExistingSidecar(archivePath)
	if !ok {
		t.Fatal("no sidecar found after Ensure")
	}
	return p
}

func TestLoadRejectsIncompleteChunkMap(t *testing.T) {
	dir := t.TempDir()
	path := twoFrameArchive(t, dir)
	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("Ensure build: %v", err)
	}
	if meta.Action != index.EnsureActionBuild || len(sc.Chunks) != 2 {
		t.Fatalf("seed Ensure: action=%s chunks=%d, want build/2", meta.Action, len(sc.Chunks))
	}
	spath := sidecarPath(t, path)
	orig := loadSidecar(t, spath)

	// Drop the second frame while preserving source identity: the review's
	// proof (Ensure returned action=load stale=false chunks=1).
	trunc := orig
	trunc.Chunks = trunc.Chunks[:1]
	writeSidecar(t, spath, trunc)
	if _, err := index.Load(spath); err == nil {
		t.Fatal("Load accepted a sidecar with a dropped frame")
	}

	// Overlapping uncompressed coverage must also be rejected.
	overlap := orig
	overlap.Chunks = append([]index.Chunk(nil), overlap.Chunks...)
	overlap.Chunks[1].UncompressedStart = overlap.Chunks[1].UncompressedEnd - 1
	writeSidecar(t, spath, overlap)
	if _, err := index.Load(spath); err == nil {
		t.Fatal("Load accepted non-contiguous uncompressed coverage")
	}

	// Ensure self-heals the invalid map back to a loadable sidecar.
	sc2, meta2, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("Ensure after tamper: %v", err)
	}
	if meta2.Action != index.EnsureActionBuild || len(sc2.Chunks) != 2 {
		t.Fatalf("heal: action=%s chunks=%d, want build/2", meta2.Action, len(sc2.Chunks))
	}
	if _, err := index.Load(spath); err != nil {
		t.Fatalf("rebuilt sidecar does not load: %v", err)
	}
}

func TestEnsureRebuildsIncompleteChunkMap(t *testing.T) {
	dir := t.TempDir()
	path := twoFrameArchive(t, dir)
	if _, _, err := index.Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatalf("seed Ensure: %v", err)
	}
	spath := sidecarPath(t, path)
	trunc := loadSidecar(t, spath)
	trunc.Chunks = trunc.Chunks[:1]
	writeSidecar(t, spath, trunc)

	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("Ensure after tamper: %v", err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %s, want build (incomplete map must regenerate)", meta.Action)
	}
	if len(sc.Chunks) != 2 {
		t.Fatalf("rebuilt chunks = %d, want 2", len(sc.Chunks))
	}
	// The rebuilt sidecar loads clean.
	if _, err := index.Load(spath); err != nil {
		t.Fatalf("rebuilt sidecar does not load: %v", err)
	}
	if !strings.Contains(spath, filepath.Base(path)) {
		t.Fatalf("unexpected sidecar path %s", spath)
	}
}

// The second review's M-08 residual: sidecars written before the chunk-map
// pin existed load fine but stay pinless forever — a frame they later lose is
// never noticed and hits drop silently on every run. A successful pinless
// load must be upgraded in place: re-serialized with the digest computed over
// the loaded chunks (no rescan), turning the legacy exposure window into one
// run.
func TestEnsureUpgradesPinlessSidecar(t *testing.T) {
	dir := t.TempDir()
	path := twoFrameArchive(t, dir)
	if _, _, err := index.Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatalf("seed Ensure: %v", err)
	}
	spath := sidecarPath(t, path)

	// Strip the pin: the legacy (pre-pinning) sidecar shape.
	legacy := loadSidecar(t, spath)
	legacy.ChunksSHA256 = ""
	writeSidecar(t, spath, legacy)

	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("Ensure on pinless sidecar: %v", err)
	}
	if meta.Action != index.EnsureActionLoad {
		t.Fatalf("action = %s, want load (upgrade must not rescan)", meta.Action)
	}
	if len(sc.Chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(sc.Chunks))
	}

	// The sidecar on disk now carries the computed pin.
	upgraded := loadSidecar(t, spath)
	if upgraded.ChunksSHA256 == "" {
		t.Fatal("pinless sidecar was not upgraded to the pinned format")
	}

	// The pin is bound to the loaded chunks: a frame lost from the upgraded
	// sidecar is now detected and rebuilt instead of dropping hits forever.
	upgraded.Chunks = upgraded.Chunks[:1]
	writeSidecar(t, spath, upgraded)
	if _, meta2, err := index.Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatalf("Ensure after tamper: %v", err)
	} else if meta2.Action != index.EnsureActionBuild {
		t.Fatalf("action = %s, want build (upgraded pin must catch the dropped frame)", meta2.Action)
	}
}
