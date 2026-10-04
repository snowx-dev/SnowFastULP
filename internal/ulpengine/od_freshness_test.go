package ulpengine

import (
	"context"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// H-21 regression tests: a dedup .idx sidecar must be bound to the content
// of the archive it was built from, not just to mtimes. Replacing an archive
// with different credentials and restoring the original mtime used to keep
// the sidecar "fresh", so re-ingesting the original credentials reported
// them as already in the library and silently emitted nothing.

// engineKeyForLine returns the current dedup key for one ULP line.
func engineKeyForLine(t *testing.T, line string) uint64 {
	t.Helper()
	host, _, login, password, ok := parseFor(line, false)
	if !ok {
		t.Fatalf("test line does not parse: %q", line)
	}
	return newLineFormatter().HashKey(host, login, password)
}

// freshPart builds the discovery-shaped archivePart for one archive.
func freshPart(t *testing.T, archive string) archivePart {
	t.Helper()
	fi, err := os.Stat(archive)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	return archivePart{
		path:        archive,
		partNum:     0,
		size:        fi.Size(),
		modTime:     fi.ModTime(),
		sidecarPath: sidecarPathForArchive(archive),
	}
}

// TestSidecarFreshnessBindsArchiveContent proves a content swap that
// preserves the archive mtime is no longer classified fresh.
func TestSidecarFreshnessBindsArchiveContent(t *testing.T) {
	dir := t.TempDir()
	archive := writeZstdArchivePath(t, dir, "20260927_bind", []string{"a.example.com:alice:pA"})
	part := freshPart(t, archive)

	if _, err := regenSidecarForPart(context.Background(), part, 1, nil, nil); err != nil {
		t.Fatalf("regen: %v", err)
	}

	// replace the archive with a different credential and restore the
	// original mtime — the exact tamper the review proved drops data.
	mt := part.modTime
	if err := writeZstdArchiveOverwrite(archive, []string{"b.example.com:bob:pB"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(archive, mt, mt); err != nil {
		t.Fatal(err)
	}
	part = freshPart(t, archive)

	st, _ := classifyPartSidecar(part, true)
	if st != sidecarStatusStale {
		t.Fatalf("status after content swap = %v, want stale", sidecarStatusName(st))
	}
}

// TestSidecarFreshnessBindsContentSameSize proves the binding is a digest,
// not just a size check: an equal-size rewrite with a preserved mtime must
// also go stale.
func TestSidecarFreshnessBindsContentSameSize(t *testing.T) {
	dir := t.TempDir()
	archive := writeZstdArchivePath(t, dir, "20260927_same", []string{"a.example.com:alice:pA"})
	part := freshPart(t, archive)

	if _, err := regenSidecarForPart(context.Background(), part, 1, nil, nil); err != nil {
		t.Fatalf("regen: %v", err)
	}

	mt := part.modTime
	// equal byte length, different credential. atomic replacement: a rename
	// over the pathname is the tamper shape the binding defends against (an
	// in-place rewrite that restores the mtime keeps the inode and is the
	// same documented residual the search index accepts).
	tmp := archive + ".swap"
	if err := writeZstdArchiveOverwrite(tmp, []string{"a.example.com:alice:pZ"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, archive); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(archive, mt, mt); err != nil {
		t.Fatal(err)
	}
	part = freshPart(t, archive)

	st, _ := classifyPartSidecar(part, true)
	if st != sidecarStatusStale {
		t.Fatalf("status after equal-size content swap = %v, want stale", sidecarStatusName(st))
	}
}

// TestSidecarStaysFreshForUnchangedArchive guards the cheap path: an archive
// that was never rewritten stays fresh, and the sidecar records the verified
// instance so later runs skip the digest pass.
func TestSidecarStaysFreshForUnchangedArchive(t *testing.T) {
	dir := t.TempDir()
	archive := writeZstdArchivePath(t, dir, "20260927_keep", []string{"a.example.com:alice:pA"})
	part := freshPart(t, archive)

	if _, err := regenSidecarForPart(context.Background(), part, 1, nil, nil); err != nil {
		t.Fatalf("regen: %v", err)
	}

	st, _ := classifyPartSidecar(part, true)
	if st != sidecarStatusFresh {
		t.Fatalf("status for unchanged archive = %v, want fresh", sidecarStatusName(st))
	}

	hdr, err := readSidecarHeader(part.sidecarPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	fi, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino, ok := archiveInstanceDevIno(fi)
	if ok {
		if hdr.verifyMtimeNano != fi.ModTime().UnixNano() || hdr.verifyDev != dev || hdr.verifyIno != ino {
			t.Fatalf("verify block = (mtime=%d dev=%d ino=%d), want (mtime=%d dev=%d ino=%d)",
				hdr.verifyMtimeNano, hdr.verifyDev, hdr.verifyIno, fi.ModTime().UnixNano(), dev, ino)
		}
	}
}

// TestFreshSidecarDedupsAgainstCurrentContent is the end-to-end version of
// the review proof: after a mtime-preserving content swap, -od must dedup
// against the NEW credential and no longer consider the OLD one in-library.
func TestFreshSidecarDedupsAgainstCurrentContent(t *testing.T) {
	dir := t.TempDir()
	lineA := "a.example.com:alice:pA"
	lineB := "b.example.com:bob:pB"
	archive := writeZstdArchivePath(t, dir, "20260927_dedup", []string{lineA})
	part := freshPart(t, archive)
	if _, err := regenSidecarForPart(context.Background(), part, 1, nil, nil); err != nil {
		t.Fatalf("regen: %v", err)
	}

	mt := part.modTime
	if err := writeZstdArchiveOverwrite(archive, []string{lineB}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(archive, mt, mt); err != nil {
		t.Fatal(err)
	}

	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      t.TempDir(),
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}

	keyA := engineKeyForLine(t, lineA)
	keyB := engineKeyForLine(t, lineB)
	idxA := int(bucketIndex(keyA, 3, true, 4))
	idxB := int(bucketIndex(keyB, 3, true, 4))
	hasB, err := destBucketHasKey(res, keyB, idxB, 4)
	if err != nil {
		t.Fatalf("gather bucket %d: %v", idxB, err)
	}
	if !hasB {
		t.Errorf("library must know the current credential after the content swap")
	}
	hasA, err := destBucketHasKey(res, keyA, idxA, 4)
	if err != nil {
		t.Fatalf("gather bucket %d: %v", idxA, err)
	}
	if hasA {
		t.Errorf("library still reports the replaced credential as already present")
	}
}

// TestLegacySidecarsRegenerateNotUpgrade pins the new lifecycle for v2/v3
// sidecars: they carry no archive identity, so they are stale by definition
// and the scan regenerates them from the archive (decompressing once).
func TestLegacySidecarsRegenerateNotUpgrade(t *testing.T) {
	dir := t.TempDir()
	lines := []string{"example.com:alice:p1", "foo.org:bob:p2"}
	archive := writeZstdArchivePath(t, dir, "20260927_legacy", lines)
	writeV2Sidecar(t, archive, []uint64{99, 1, 42, 1})
	part := freshPart(t, archive)

	if st, _ := classifyPartSidecar(part, true); st != sidecarStatusStale {
		t.Fatalf("status for legacy v2 sidecar = %v, want stale", sidecarStatusName(st))
	}

	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      t.TempDir(),
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesRegen != 1 {
		t.Fatalf("ArchivesRegen = %d, want 1 (legacy sidecars must regenerate)", res.ArchivesRegen)
	}
	for _, line := range lines {
		k := engineKeyForLine(t, line)
		idx := int(bucketIndex(k, 3, true, 4))
		ok, err := destBucketHasKey(res, k, idx, 4)
		if err != nil {
			t.Fatalf("gather bucket %d: %v", idx, err)
		}
		if !ok {
			t.Errorf("regenerated sidecar missing key for %q", line)
		}
	}
}

// writeZstdArchivePath mirrors helperWriteArchive but returns the path.
func writeZstdArchivePath(t *testing.T, dir, stamp string, lines []string) string {
	t.Helper()
	return helperWriteArchive(t, dir, stamp, lines)
}

// writeZstdArchiveOverwrite replaces an archive's content in place.
func writeZstdArchiveOverwrite(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(f)
	if err != nil {
		f.Close()
		return err
	}
	for _, ln := range lines {
		if _, err := enc.Write([]byte(ln + "\n")); err != nil {
			enc.Close()
			f.Close()
			return err
		}
	}
	if err := enc.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// TestVerifyCacheBackfilledAfterDigestPass proves the digest pass on first
// sight of an archive instance records the confirmed instance so later runs
// take the cheap path again.
func TestVerifyCacheBackfilledAfterDigestPass(t *testing.T) {
	dir := t.TempDir()
	archive := writeZstdArchivePath(t, dir, "20260927_backfill", []string{"a.example.com:alice:pA"})
	part := freshPart(t, archive)

	if _, err := regenSidecarForPart(context.Background(), part, 1, nil, nil); err != nil {
		t.Fatalf("regen: %v", err)
	}

	// wipe the verify cache: the next classify must re-digest, confirm, and
	// backfill the cache in place
	path := part.sidecarPath
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	var zero [24]byte
	if _, err := f.WriteAt(zero[:], sidecarBaseHeaderBytes+40); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if st, _ := classifyPartSidecar(part, true); st != sidecarStatusFresh {
		t.Fatalf("status after digest pass = %v, want fresh", sidecarStatusName(st))
	}
	hdr, err := readSidecarHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	dev, ino, ok := archiveInstanceDevIno(fi)
	if ok && (hdr.verifyMtimeNano != fi.ModTime().UnixNano() || hdr.verifyDev != dev || hdr.verifyIno != ino) {
		t.Fatalf("verify cache not backfilled: got (mtime=%d dev=%d ino=%d)",
			hdr.verifyMtimeNano, hdr.verifyDev, hdr.verifyIno)
	}
}
