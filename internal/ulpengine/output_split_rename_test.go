package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRotateStagesSidecarsUnderPartName: when part 1's final name gains the
// _part1 suffix at rotation, its staged search sidecar (and .idx) must target
// the new part name; nothing final is published until commit, and after commit
// the sidecars exist only under the part names.
func TestRotateStagesSidecarsUnderPartName(t *testing.T) {
	dir := t.TempDir()
	stamp := "20260920_split"

	sink, err := newChunkedZstdSink(dir, stamp, 1, nil, true, true)
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}

	if err := sink.writeBatch([]byte("https://a.example.com:u:p\n"), 1, nil); err != nil {
		t.Fatalf("write part1: %v", err)
	}
	// second line forces a rotation: part1's final name becomes _part1
	if err := sink.writeBatch([]byte("https://b.example.com:u:p\n"), 1, nil); err != nil {
		t.Fatalf("write part2: %v", err)
	}
	if err := sink.seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}

	part1 := zstPartPath(dir, stamp, 1)
	oldName := filepath.Join(dir, DefaultBasename(stamp)+".txt.zst")

	// nothing published before commit, under either name
	for _, a := range []string{oldName, part1, zstPartPath(dir, stamp, 2)} {
		if _, err := os.Stat(a); !os.IsNotExist(err) {
			t.Fatalf("archive %s published before commit: %v", filepath.Base(a), err)
		}
	}
	for _, a := range []string{oldName, part1, zstPartPath(dir, stamp, 2)} {
		for _, s := range []string{sidecarPathForArchive(a), searchSidecarPathForArchive(a)} {
			if _, err := os.Stat(s); !os.IsNotExist(err) {
				t.Fatalf("sidecar %s published before commit: %v", filepath.Base(s), err)
			}
		}
	}
	// staged temps are cleanup-registered until commit
	if len(SnapshotCleanupPaths()) == 0 {
		t.Error("staged temps not cleanup-registered before commit")
	}

	if err := sink.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// search sidecar published under the final part-1 name, never under the
	// pre-rotation name
	if _, err := os.Stat(searchSidecarPathForArchive(part1)); err != nil {
		t.Fatalf("committed search sidecar missing: %v", err)
	}
	if _, err := os.Stat(searchSidecarPathForArchive(oldName)); !os.IsNotExist(err) {
		t.Errorf("search sidecar present under pre-rotation name: %v", err)
	}
	// .idx follows too
	if _, err := os.Stat(sidecarPathForArchive(part1)); err != nil {
		t.Fatalf("committed .idx sidecar missing: %v", err)
	}
	if _, err := os.Stat(sidecarPathForArchive(oldName)); !os.IsNotExist(err) {
		t.Errorf(".idx sidecar present under pre-rotation name: %v", err)
	}
	// committed finals are not cleanup candidates
	for _, p := range SnapshotCleanupPaths() {
		if p == searchSidecarPathForArchive(part1) || p == sidecarPathForArchive(part1) {
			t.Errorf("committed sidecar still cleanup-registered: %s", p)
		}
	}

	// lines survived the rotation + publish in both parts
	for i, path := range []string{part1, zstPartPath(dir, stamp, 2)} {
		lines := readZstdLines(t, path)
		if len(lines) != 1 {
			t.Errorf("part%d lines = %d, want 1", i+1, len(lines))
		}
	}
}

// TestRotateToleratesMissingSearchSidecar: rotation with an unindexed part
// (no search sidecar on disk) must not fail the rotate.
func TestRotateToleratesMissingSearchSidecar(t *testing.T) {
	dir := t.TempDir()
	stamp := "20260920_split2"

	sink, err := newChunkedZstdSink(dir, stamp, 1, nil, false, true)
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	if err := sink.writeBatch([]byte("https://a.example.com:u:p\n"), 1, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sink.writeBatch([]byte("https://b.example.com:u:p\n"), 1, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sink.seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := sink.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := os.Stat(zstPartPath(dir, stamp, 2)); err != nil {
		t.Fatalf("part2 missing: %v", err)
	}
}

// TestRegenSkipsVanishedArchive: an archive deleted between discovery and
// regen (manual change concurrent with the run; our own runs hold the library
// lock so they cannot do this) must be reported as a skipped part, not a
// fatal regen failure.
func TestRegenSkipsVanishedArchive(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_vanish", 2, 5)

	// delete part 1's archive after its sidecar-less discovery snapshot;
	// regen must skip it and still regen part 2.
	if err := os.Remove(parts[0].path); err != nil {
		t.Fatal(err)
	}

	paths, err := regenParts(context.Background(), parts, odConfig{Debug: nil}, nil)
	if err != nil {
		t.Fatalf("regenParts: %v", err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "sfu_vanish_part1.txt.zst" {
		t.Fatalf("skipped paths = %v, want the vanished part1", paths)
	}
}
