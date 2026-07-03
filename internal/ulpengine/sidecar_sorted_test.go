package ulpengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// sidecarBucketKeys opens, range-reads one bucket, and closes — a one-shot
// convenience for tests. Hot paths reuse a sidecarReader across buckets.
func sidecarBucketKeys(path string, bucketIdx, numBuckets int) ([]uint64, error) {
	sr, err := openSidecarReader(path)
	if err != nil {
		return nil, err
	}
	defer sr.close()
	return sr.bucketKeys(bucketIdx, numBuckets)
}

// writeV2Sidecar writes a legacy unsorted v2 .idx fixture for migration tests.
func writeV2Sidecar(t *testing.T, archivePath string, keys []uint64) {
	t.Helper()
	if err := ensureIdxSubdir(filepath.Dir(archivePath)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	var h [sidecarHeaderBytes]byte
	copy(h[0:4], sidecarMagic)
	binary.LittleEndian.PutUint16(h[4:6], sidecarFormatV2)
	binary.LittleEndian.PutUint16(h[6:8], sidecarHashAlgoXX)
	binary.LittleEndian.PutUint64(h[8:16], uint64(len(keys)))
	binary.LittleEndian.PutUint64(h[16:24], parserVersion)
	buf.Write(h[:])
	var kb [SidecarKeyBytes]byte
	for _, k := range keys {
		binary.LittleEndian.PutUint64(kb[:], k)
		buf.Write(kb[:])
	}
	if err := os.WriteFile(sidecarPathForArchive(archivePath), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// transparent migration: a legacy v2 sidecar is re-sorted to v3 in place
// (no archive needed), preserving the exact key set.
func TestUpgradeV2SidecarToV3(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_v2.txt.zst")
	keys := []uint64{500, 3, 3, 1 << 63, 42, 500}
	writeV2Sidecar(t, archive, keys)
	path := sidecarPathForArchive(archive)

	if hdr, err := readSidecarHeader(path); err != nil || hdr.sorted() {
		t.Fatalf("fixture should be readable v2 (unsorted); err=%v", err)
	}
	if _, err := upgradeSidecarToV3(context.Background(), path); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	hdr, err := readSidecarHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hdr.sorted() || hdr.formatVersion != sidecarFormatV3 {
		t.Fatalf("expected v3 after upgrade, got v%d", hdr.formatVersion)
	}
	got := readAllSidecarKeys(t, path)
	assertSortedUnique(t, got)
	want := slices.Clone(keys)
	slices.Sort(want)
	want = slices.Compact(want)
	if !sliceEqualUint64(got, want) {
		t.Fatalf("upgraded keys = %v, want %v", got, want)
	}
}

// reads every key from a sidecar in file order.
func readAllSidecarKeys(t *testing.T, path string) []uint64 {
	t.Helper()
	var out []uint64
	if err := streamSidecarKeys(path, func(k uint64) error {
		out = append(out, k)
		return nil
	}); err != nil {
		t.Fatalf("streamSidecarKeys: %v", err)
	}
	return out
}

func assertSortedUnique(t *testing.T, keys []uint64) {
	t.Helper()
	for i := 1; i < len(keys); i++ {
		if keys[i] <= keys[i-1] {
			t.Fatalf("not strictly ascending at %d: %d then %d", i, keys[i-1], keys[i])
		}
	}
}

// in-RAM path: unsorted input with dups -> sorted, deduped v3 body.
func TestSidecarWriterSortsAndDedups(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_a.txt.zst")
	in := []uint64{42, 7, 7, 1000, 1, 42, 99, 1, 0}
	count := writeSidecarKeysForTest(t, archive, in)

	want := slices.Clone(in)
	slices.Sort(want)
	want = slices.Compact(want)
	if count != uint64(len(want)) {
		t.Fatalf("count = %d, want %d", count, len(want))
	}
	got := readAllSidecarKeys(t, sidecarPathForArchive(archive))
	assertSortedUnique(t, got)
	if !sliceEqualUint64(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	hdr, err := readSidecarHeader(sidecarPathForArchive(archive))
	if err != nil {
		t.Fatalf("readSidecarHeader: %v", err)
	}
	if !hdr.sorted() || hdr.formatVersion != sidecarFormatV3 {
		t.Fatalf("expected v3 sorted header, got v%d", hdr.formatVersion)
	}
}

// external-merge path: force spills with a tiny budget, result must still be
// globally sorted + deduped across runs.
func TestSidecarWriterExternalMerge(t *testing.T) {
	old := sidecarSortMaxKeys
	sidecarSortMaxKeys = 4 // spill every 4 keys
	defer func() { sidecarSortMaxKeys = old }()

	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_big.txt.zst")
	in := []uint64{}
	for i := 0; i < 50; i++ {
		in = append(in, uint64((i*37)%23)) // lots of cross-batch dups, unsorted
	}
	count := writeSidecarKeysForTest(t, archive, in)

	want := slices.Clone(in)
	slices.Sort(want)
	want = slices.Compact(want)
	if count != uint64(len(want)) {
		t.Fatalf("merged count = %d, want %d", count, len(want))
	}
	got := readAllSidecarKeys(t, sidecarPathForArchive(archive))
	assertSortedUnique(t, got)
	if !sliceEqualUint64(got, want) {
		t.Fatalf("merged keys = %v, want %v", got, want)
	}
}

// range reader returns exactly each bucket's keys (top-bits partition), and the
// union over all buckets reconstructs the full set.
func TestSidecarBucketKeysRangeRead(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_r.txt.zst")
	keys := []uint64{
		0, 1, 0x4000000000000000, 0x7fffffffffffffff,
		0x8000000000000000, 0xc000000000000000, 0xffffffffffffffff,
		123456789, 0xdeadbeefcafebabe,
	}
	writeSidecarKeysForTest(t, archive, keys)
	path := sidecarPathForArchive(archive)

	const B = 4
	mask := uint64(B - 1)
	var union []uint64
	for b := 0; b < B; b++ {
		got, err := sidecarBucketKeys(path, b, B)
		if err != nil {
			t.Fatalf("bucket %d: %v", b, err)
		}
		assertSortedUnique(t, got)
		for _, k := range got {
			if int(bucketIndex(k, mask, true, B)) != b {
				t.Fatalf("key %d in bucket %d but bucketIndex=%d", k, b, bucketIndex(k, mask, true, B))
			}
		}
		union = append(union, got...)
	}
	slices.Sort(union)
	want := slices.Clone(keys)
	slices.Sort(want)
	want = slices.Compact(want)
	if !sliceEqualUint64(union, want) {
		t.Fatalf("union over buckets = %v, want %v", union, want)
	}
}

// external-merge during in-place upgrade: huge v2 sidecar must still become sorted v3.
func TestUpgradeV2SidecarToV3ExternalMerge(t *testing.T) {
	old := sidecarSortMaxKeys
	sidecarSortMaxKeys = 4
	defer func() { sidecarSortMaxKeys = old }()

	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_big.txt.zst")
	keys := make([]uint64, 0, 80)
	for i := 0; i < 80; i++ {
		keys = append(keys, uint64((i*17)%31)) // unsorted + cross-batch dups
	}
	writeV2Sidecar(t, archive, keys)
	path := sidecarPathForArchive(archive)

	if _, err := upgradeSidecarToV3(context.Background(), path); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got := readAllSidecarKeys(t, path)
	assertSortedUnique(t, got)
	want := slices.Clone(keys)
	slices.Sort(want)
	want = slices.Compact(want)
	if !sliceEqualUint64(got, want) {
		t.Fatalf("upgraded keys mismatch: got %d want %d", len(got), len(want))
	}
	hdr, err := readSidecarHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hdr.sorted() || hdr.formatVersion != sidecarFormatV3 {
		t.Fatalf("expected v3 after spill upgrade, got v%d", hdr.formatVersion)
	}
}

// Ctrl+C during migration: a cancelled upgrade must abort without touching the
// original v2 sidecar — the library is never left half-written.
func TestUpgradeSidecarToV3CancelLeavesOriginalIntact(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_c.txt.zst")
	keys := []uint64{9, 1, 5, 1, 7}
	writeV2Sidecar(t, archive, keys)
	path := sidecarPathForArchive(archive)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate Ctrl+C before the upgrade gets going
	if _, uerr := upgradeSidecarToV3(ctx, path); uerr == nil {
		t.Fatal("cancelled upgrade should return an error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("original sidecar missing after cancelled upgrade: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("original v2 sidecar was modified by a cancelled upgrade")
	}
	if hdr, herr := readSidecarHeader(path); herr != nil || hdr.sorted() {
		t.Fatalf("sidecar should still be intact v2 after cancel; err=%v", herr)
	}
	// no leftover temp in the idx dir
	tmps, _ := filepath.Glob(filepath.Join(dir, idxSubdirName, "*.tmp"))
	if len(tmps) != 0 {
		t.Errorf("cancelled upgrade left temp files: %v", tmps)
	}
}

// mid-stream cancel on a large v2 sidecar: must abort without modifying the original.
func TestUpgradeSidecarToV3CancelMidStreamLeavesOriginalIntact(t *testing.T) {
	oldMask := sidecarUpgradeCancelCheckMask
	sidecarUpgradeCancelCheckMask = 0xfff // poll often so cancel wins the race in tests
	defer func() { sidecarUpgradeCancelCheckMask = oldMask }()

	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_big.txt.zst")
	const nKeys = 50_000
	keys := make([]uint64, nKeys)
	for i := range keys {
		keys[i] = uint64((i * 17) % 999983)
	}
	writeV2Sidecar(t, archive, keys)
	path := sidecarPathForArchive(archive)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Deterministically cancel mid-stream: the first poll fires at key 0, so
	// cancelling on the second poll guarantees a batch of keys was already
	// written to the temp before the upgrade aborts (no wall-clock race).
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

	if _, uerr := upgradeSidecarToV3(ctx, path); uerr == nil {
		t.Fatal("cancelled mid-stream upgrade should return an error")
	}
	if polls < 2 {
		t.Fatalf("upgrade finished after %d poll(s); cancel never landed mid-stream", polls)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("original sidecar missing after cancelled upgrade: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("original v2 sidecar was modified by a mid-stream cancelled upgrade")
	}
	if hdr, herr := readSidecarHeader(path); herr != nil || hdr.sorted() {
		t.Fatalf("sidecar should still be intact v2 after mid-stream cancel; err=%v", herr)
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, idxSubdirName, "*.tmp"))
	if len(tmps) != 0 {
		t.Errorf("cancelled upgrade left temp files: %v", tmps)
	}
}
