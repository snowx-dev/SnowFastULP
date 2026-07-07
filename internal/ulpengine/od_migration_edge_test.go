package ulpengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mixed library: v2 (upgrade), v3 (fresh), missing (regen), stale archive (regen).
func TestRunODScanMixedLibrarySidecarStates(t *testing.T) {
	dir := t.TempDir()
	tempDir := t.TempDir()
	past := time.Now().Add(-time.Hour)

	// v2 → upgrade in place
	v2Archive := filepath.Join(dir, "sfu_v2.txt.zst")
	if err := os.WriteFile(v2Archive, []byte("v2 archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeV2Sidecar(t, v2Archive, []uint64{10, 3, 7})
	if err := os.Chtimes(v2Archive, past, past); err != nil {
		t.Fatal(err)
	}

	// v3 fresh → no work
	v3Archive := filepath.Join(dir, "sfu_v3.txt.zst")
	if err := os.WriteFile(v3Archive, []byte("v3 archive"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		Dest:            dir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 4 {
		t.Fatalf("ArchivesTotal = %d, want 4", res.ArchivesTotal)
	}
	if res.ArchivesRegen != 2 {
		t.Errorf("ArchivesRegen = %d, want 2 (missing + stale)", res.ArchivesRegen)
	}
	if res.ArchivesUpgraded != 1 {
		t.Errorf("ArchivesUpgraded = %d, want 1", res.ArchivesUpgraded)
	}
	// v3 untouched + v2 run (upgraded in place, no regen) both count as fresh runs
	if res.ArchivesFresh != 2 {
		t.Errorf("ArchivesFresh = %d, want 2 (v3 + upgraded v2 run)", res.ArchivesFresh)
	}

	v2Path := sidecarPathForArchive(v2Archive)
	hdr, err := readSidecarHeader(v2Path)
	if err != nil || !hdr.sorted() {
		t.Errorf("v2 archive sidecar not upgraded to v3: err=%v", err)
	}
}

// archive touched after v2 sidecar was written → stale (regen), not in-place upgrade.
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
	if st, _ := classifyPartSidecar(part); st != sidecarStatusStale {
		t.Errorf("status = %v, want stale when archive newer than v2 sidecar", st)
	}
}

// truncated v2 body: upgrade must fail and leave the original sidecar byte-identical.
func TestUpgradeSidecarToV3TruncatedBodyFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_trunc.txt.zst")
	path := sidecarPathForArchive(archive)
	if err := ensureIdxSubdir(dir); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	var h [sidecarHeaderBytes]byte
	copy(h[0:4], sidecarMagic)
	binary.LittleEndian.PutUint16(h[4:6], sidecarFormatV2)
	binary.LittleEndian.PutUint16(h[6:8], sidecarHashAlgoXX)
	binary.LittleEndian.PutUint64(h[8:16], 100) // claim 100 keys
	binary.LittleEndian.PutUint64(h[16:24], parserVersion)
	buf.Write(h[:])
	// write only 3 keys, not 100
	for _, k := range []uint64{1, 2, 3} {
		var kb [8]byte
		binary.LittleEndian.PutUint64(kb[:], k)
		buf.Write(kb[:])
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, uerr := upgradeSidecarToV3(context.Background(), path); uerr == nil {
		t.Fatal("expected error upgrading truncated v2 sidecar")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("truncated v2 sidecar was modified after failed upgrade")
	}
}

// empty v2 sidecar (zero keys) upgrades cleanly to empty v3.
func TestUpgradeV2SidecarEmptyToV3(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_empty.txt.zst")
	writeV2Sidecar(t, archive, nil)
	path := sidecarPathForArchive(archive)

	count, err := upgradeSidecarToV3(context.Background(), path)
	if err != nil {
		t.Fatalf("upgrade empty v2: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	hdr, err := readSidecarHeader(path)
	if err != nil || !hdr.sorted() || hdr.keyCount != 0 {
		t.Errorf("empty v3 sidecar bad: hdr=%+v err=%v", hdr, err)
	}
}

// odScan upgrade: cancel mid-flight on a single large part leaves that sidecar intact.
// (parallel pool may finish sibling parts before cancel; re-run upgrades the rest.)
func TestRunODScanUpgradeCancelMidStream(t *testing.T) {
	oldMask := sidecarUpgradeCancelCheckMask
	sidecarUpgradeCancelCheckMask = 0xfff
	defer func() { sidecarUpgradeCancelCheckMask = oldMask }()

	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_cancel.txt.zst")
	if err := os.WriteFile(archive, []byte("single part"), 0o644); err != nil {
		t.Fatal(err)
	}
	const keysPerPart = 50_000
	keys := make([]uint64, keysPerPart)
	for j := range keys {
		keys[j] = uint64(j * 31 % 999983)
	}
	writeV2Sidecar(t, archive, keys)
	path := sidecarPathForArchive(archive)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(archive, past, past); err != nil {
		t.Fatal(err)
	}

	// Cancel deterministically once the upgrade is mid-stream (second poll),
	// rather than racing a wall-clock sleep against a fast in-RAM migration.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var polls int
	sidecarUpgradeOnCancelCheck = func() {
		polls++
		if polls == 2 {
			cancel()
		}
	}
	defer func() { sidecarUpgradeOnCancelCheck = nil }()

	_, err = runODScan(ctx, odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         t.TempDir(),
		Workers:         1,
	}, &ODMetrics{})
	if err == nil {
		t.Fatal("expected cancel during upgrade scan")
	}
	if polls < 2 {
		t.Fatalf("upgrade finished after %d poll(s); cancel never landed mid-stream", polls)
	}

	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("sidecar missing after cancel: %v", rerr)
	}
	if !bytes.Equal(before, got) {
		t.Fatal("sidecar modified after cancel")
	}
	if hdr, herr := readSidecarHeader(path); herr != nil || hdr.sorted() {
		t.Fatalf("sidecar should remain v2 after cancel; err=%v", herr)
	}
}
