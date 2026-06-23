package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A failed/cancelled run discards its output via removeOutputFiles. It must drop
// the committed sidecars alongside the archive, so a later -od run never reads an
// orphan .idx pointing at a removed archive.
func TestRemoveOutputFilesAlsoRemovesSidecars(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_x.txt.zst")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := sidecarPathForArchive(archive)
	searchSidecar := searchSidecarPathForArchive(archive)
	for _, p := range []string{sidecar, searchSidecar} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("idx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removeOutputFiles([]string{archive})

	for _, p := range []string{archive, sidecar, searchSidecar} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s removed; stat err = %v", filepath.Base(p), err)
		}
	}
}
