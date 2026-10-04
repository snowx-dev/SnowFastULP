package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mixed library: legacy v2 (regen), identity-bound v3-era writer output
// (fresh), missing (regen), stale archive (regen). v2 sidecars carry no
// archive-identity binding (H-21), so they regenerate from their archives —
// the archives must be real zstd payloads for the regen to succeed.
func TestRunODScanMixedLibrarySidecarStates(t *testing.T) {
	dir := t.TempDir()
	tempDir := t.TempDir()
	past := time.Now().Add(-time.Hour)

	// legacy v2 sidecar → regen from archive content
	v2Archive := filepath.Join(dir, "sfu_v2.txt.zst")
	writeZstdArchive(t, v2Archive, []string{"v2.example.com:u1:p1"})
	writeV2Sidecar(t, v2Archive, []uint64{10, 3, 7})
	if err := os.Chtimes(v2Archive, past, past); err != nil {
		t.Fatal(err)
	}

	// identity-bound sidecar (current writer output) → no work
	v3Archive := filepath.Join(dir, "sfu_v3.txt.zst")
	writeZstdArchive(t, v3Archive, []string{"v3.example.com:u1:p1"})
	writeSidecarKeysForTest(t, v3Archive, []uint64{1, 2, 3})
	if err := os.Chtimes(v3Archive, past, past); err != nil {
		t.Fatal(err)
	}

	// missing sidecar → regen from archive content
	missArchive := filepath.Join(dir, "sfu_miss.txt.zst")
	writeZstdArchive(t, missArchive, []string{"example.com:alice:p1", "foo.org:bob:p2"})

	// archive newer than sidecar → stale regen (valid zst so regen succeeds)
	staleArchive := filepath.Join(dir, "sfu_stale.txt.zst")
	writeZstdArchive(t, staleArchive, []string{"stale.example.com:u:p"})
	writeV2Sidecar(t, staleArchive, []uint64{99})
	pastSidecar := time.Now().Add(-time.Hour)
	if err := os.Chtimes(sidecarPathForArchive(staleArchive), pastSidecar, pastSidecar); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleArchive, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 4 {
		t.Fatalf("ArchivesTotal = %d, want 4", res.ArchivesTotal)
	}
	if res.ArchivesRegen != 3 {
		t.Errorf("ArchivesRegen = %d, want 3 (legacy v2 + missing + stale)", res.ArchivesRegen)
	}
	if res.ArchivesUpgraded != 0 {
		t.Errorf("ArchivesUpgraded = %d, want 0 (no in-place upgrade anymore)", res.ArchivesUpgraded)
	}
	if res.ArchivesFresh != 1 {
		t.Errorf("ArchivesFresh = %d, want 1 (the identity-bound run)", res.ArchivesFresh)
	}

	// the legacy part came out as a bound v4 sidecar
	v2Path := sidecarPathForArchive(v2Archive)
	hdr, err := readSidecarHeader(v2Path)
	if err != nil {
		t.Errorf("v2 archive sidecar not regenerated as v4: %v", err)
	} else if hdr.formatVersion != sidecarFormatV4 {
		t.Errorf("v2 archive sidecar formatVersion = %d, want %d", hdr.formatVersion, sidecarFormatV4)
	}
}

// archive touched after a legacy sidecar was written → stale (regen).
func TestClassifyPartSidecarArchiveNewerThanV2IsStale(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_x.txt.zst")
	if err := os.WriteFile(archive, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeV2Sidecar(t, archive, []uint64{1, 2})
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(archive, future, future); err != nil {
		t.Fatal(err)
	}
	part := archivePart{
		path:        archive,
		sidecarPath: sidecarPathForArchive(archive),
	}
	if fi, err := os.Stat(archive); err == nil {
		part.modTime = fi.ModTime()
	}
	if st, _ := classifyPartSidecar(part, true); st != sidecarStatusStale {
		t.Errorf("status = %v, want stale when archive newer than v2 sidecar", st)
	}
}
