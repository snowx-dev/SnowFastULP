package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// every shape writer can emit + bait shapes that must be rejected
func TestParseArchiveName(t *testing.T) {
	cases := []struct {
		path      string
		wantRunID string
		wantPart  int
	}{
		{"sfu_20260514_abc.txt.zst", "sfu_20260514_abc", 0},
		{"sfu_20260514_abc_part1.txt.zst", "sfu_20260514_abc", 1},
		{"sfu_20260514_abc_part42.txt.zst", "sfu_20260514_abc", 42},
		{"/libs/sfu_xyz.txt.zst", "sfu_xyz", 0},
		{"foreign.zst", "", 0},
		{"sfu_xyz.zst", "", 0}, // missing .txt
		{"sfu_xyz.txt", "", 0}, // missing .zst
		// non-numeric _partXYZ = treat whole thing as runID stem
		{"sfu_xyz_partabc.txt.zst", "sfu_xyz_partabc", 0},
	}
	for _, c := range cases {
		gotID, gotPart := parseArchiveName(c.path)
		if gotID != c.wantRunID || gotPart != c.wantPart {
			t.Errorf("parseArchiveName(%q) = (%q, %d), want (%q, %d)",
				c.path, gotID, gotPart, c.wantRunID, c.wantPart)
		}
	}
}

// 2 single-archive runs + 1 multi-part + foreign noise + self-stamp.
// parts grouped/sorted, foreign filtered, self excluded
func TestDiscoverArchiveRuns(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"sfu_a.txt.zst",
		"sfu_b_part2.txt.zst",
		"sfu_b_part1.txt.zst",
		"sfu_b_part3.txt.zst",
		"sfu_c.txt.zst",
		"sfu_self.txt.zst", // excluded by stamp
		"foreign.zst",
		"random_text.txt",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := discoverArchiveRuns(dir, "sfu_self")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	gotIDs := make([]string, len(runs))
	for i, r := range runs {
		gotIDs[i] = r.runID
	}
	sort.Strings(gotIDs)
	wantIDs := []string{"sfu_a", "sfu_b", "sfu_c"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("runIDs = %v, want %v", gotIDs, wantIDs)
	}

	// sfu_b: 3 parts in numeric order
	var bRun archiveRun
	for _, r := range runs {
		if r.runID == "sfu_b" {
			bRun = r
		}
	}
	if len(bRun.parts) != 3 {
		t.Fatalf("sfu_b parts = %d, want 3", len(bRun.parts))
	}
	for i, p := range bRun.parts {
		if p.partNum != i+1 {
			t.Errorf("part %d num = %d, want %d", i, p.partNum, i+1)
		}
	}
	wantSidecar := sidecarPathForArchive(filepath.Join(dir, "sfu_b_part1.txt.zst"))
	if got := bRun.parts[0].sidecarPath; got != wantSidecar {
		t.Errorf("parts[0].sidecarPath = %q, want %q", got, wantSidecar)
	}
}

// walks every status: fresh, missing, stale-mtime, stale-version
func TestClassifyPartSidecar(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_x.txt.zst")
	if err := os.WriteFile(archive, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	part := archivePart{
		path:        archive,
		partNum:     0,
		modTime:     time.Now().Add(-time.Hour),
		sidecarPath: sidecarPathForArchive(archive),
	}
	if fi, err := os.Stat(archive); err == nil {
		part.modTime = fi.ModTime()
	}

	if st, _ := classifyPartSidecar(part); st != sidecarStatusMissing {
		t.Errorf("status = %v, want missing", st)
	}

	writeSidecarKeysForTest(t, archive, nil)
	if st, _ := classifyPartSidecar(part); st != sidecarStatusFresh {
		t.Errorf("status = %v, want fresh", st)
	}

	// archive newer than sidecar => stale
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(archive, future, future); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(archive); err == nil {
		part.modTime = fi.ModTime()
	}
	if st, _ := classifyPartSidecar(part); st != sidecarStatusStale {
		t.Errorf("status (touched) = %v, want stale", st)
	}

	// corrupt parserVersion => stale
	if err := os.Chtimes(archive, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(archive); err == nil {
		part.modTime = fi.ModTime()
	}
	hdr := makeSidecarHeader(0)
	for i := 16; i < 24; i++ {
		hdr[i] = 0xff
	}
	if err := os.WriteFile(sidecarPathForArchive(archive), hdr[:], 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := classifyPartSidecar(part); st != sidecarStatusStale {
		t.Errorf("status (bad version) = %v, want stale", st)
	}
}

// empty dest is no-op, no dest_keys subdir. -od on empty == -o
func TestRunODScanEmpty(t *testing.T) {
	dir := t.TempDir()
	tempDir := t.TempDir()
	res, err := runODScanSync(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &odMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 0 {
		t.Errorf("ArchivesTotal = %d, want 0", res.ArchivesTotal)
	}
	if len(res.DestKeyBucketPaths) != 0 {
		t.Errorf("DestKeyBucketPaths len = %d, want 0", len(res.DestKeyBucketPaths))
	}
	// no work = no dest_keys/ setup
	if _, err := os.Stat(filepath.Join(tempDir, "dest_keys")); !os.IsNotExist(err) {
		t.Errorf("dest_keys/ should not exist when scan found nothing")
	}
}

// fresh sidecar exists, no regen. dest_keys buckets get the hashes
func TestRunODScanWithFreshSidecar(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_prev.txt.zst")
	if err := os.WriteFile(archive, []byte("dummy archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys := []uint64{1, 2, 100, 200, 0xdeadbeefcafebabe}
	writeSidecarKeysForTest(t, archive, keys)
	// sidecar mtime > archive mtime, age the archive
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(archive, past, past)

	tempDir := t.TempDir()
	res, err := runODScanSync(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &odMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 1 || res.ArchivesFresh != 1 || res.ArchivesRegen != 0 {
		t.Errorf("counts = total=%d fresh=%d regen=%d, want 1/1/0",
			res.ArchivesTotal, res.ArchivesFresh, res.ArchivesRegen)
	}
	if res.TotalKeysLoaded != uint64(len(keys)) {
		t.Errorf("keysLoaded = %d, want %d", res.TotalKeysLoaded, len(keys))
	}

	for _, k := range keys {
		idx := int(k % 4)
		bp := res.DestKeyBucketPaths[idx]
		if bp == "" {
			t.Errorf("bucket %d for key %d is empty, expected entry", idx, k)
			continue
		}
		if !bucketContainsKey(t, bp, k) {
			t.Errorf("bucket %d missing key %d", idx, k)
		}
	}
}

// archive but no sidecar, must stream + hash + write + load.
// "user deleted .idx" / parserVersion bump recovery flow
func TestRunODScanRegenerates(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_prev.txt.zst")
	credLines := []string{
		"example.com:alice:p1",
		"foo.org:bob:p2",
		"baz.net:charlie:p3",
	}
	writeZstdArchive(t, archive, credLines)

	tempDir := t.TempDir()
	res, err := runODScanSync(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &odMetrics{})
	if err != nil {
		t.Fatalf("runODScan regen: %v", err)
	}
	if res.ArchivesRegen != 1 {
		t.Errorf("ArchivesRegen = %d, want 1", res.ArchivesRegen)
	}
	if res.TotalKeysLoaded != uint64(len(credLines)) {
		t.Errorf("keysLoaded = %d, want %d", res.TotalKeysLoaded, len(credLines))
	}

	if _, err := os.Stat(sidecarPathForArchive(archive)); err != nil {
		t.Errorf("expected sidecar on disk after regen: %v", err)
	}

	fmtr := newLineFormatter()
	for _, line := range credLines {
		host, _, login, password, ok := parseFor(line, false)
		if !ok {
			t.Fatalf("parse failed for %q (test setup bug)", line)
		}
		h := fmtr.HashKey(host, login, password)
		idx := int(h % 4)
		bp := res.DestKeyBucketPaths[idx]
		if bp == "" {
			t.Errorf("bucket %d empty for %q", idx, line)
			continue
		}
		if !bucketContainsKey(t, bp, h) {
			t.Errorf("bucket %d missing hash for %q", idx, line)
		}
	}
}

// 1 good + 1 garbage archive. skip the bad one w/ warning, dont fail
func TestRunODScanSkipsCorruptArchive(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "sfu_good.txt.zst")
	bad := filepath.Join(dir, "sfu_bad.txt.zst")
	writeZstdArchive(t, good, []string{"example.com:alice:p1"})
	if err := os.WriteFile(bad, []byte("not a real zst archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	res, err := runODScanSync(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &odMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 2 {
		t.Errorf("total = %d, want 2", res.ArchivesTotal)
	}
	if res.ArchivesSkipped != 1 {
		t.Errorf("skipped = %d, want 1", res.ArchivesSkipped)
	}
	if res.TotalKeysLoaded != 1 {
		t.Errorf("keysLoaded = %d, want 1", res.TotalKeysLoaded)
	}
}

// .zst w/ one cred per line, fakes a past run for od_scan tests
func writeZstdArchive(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range lines {
		if _, err := enc.Write([]byte(ln + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
}

// linear scan, bucket files are tiny in tests
func bucketContainsKey(t *testing.T, path string, want uint64) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bucket: %v", err)
	}
	if len(data)%sidecarKeyBytes != 0 {
		t.Fatalf("bucket %s size %d not multiple of %d", path, len(data), sidecarKeyBytes)
	}
	for i := 0; i < len(data); i += sidecarKeyBytes {
		k := bytesLEUint64(data[i : i+sidecarKeyBytes])
		if k == want {
			return true
		}
	}
	return false
}

func bytesLEUint64(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// keep bytes import warm
var _ = bytes.Equal
