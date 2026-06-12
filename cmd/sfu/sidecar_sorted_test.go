package main

import (
	"path/filepath"
	"slices"
	"testing"
)

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
		got, err := sidecarBucketKeys(path, b, mask, true, B)
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
