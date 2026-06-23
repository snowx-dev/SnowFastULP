package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// readSidecarKeysOrdered streams a sidecar's keys and asserts the v3 invariants
// the migration must guarantee: the body is strictly ascending (sorted) and
// therefore duplicate-free. Returns the keys for set comparison by the caller.
func readSidecarKeysOrdered(t *testing.T, path string) []uint64 {
	t.Helper()
	var got []uint64
	var prev uint64
	var have bool
	if err := streamSidecarKeys(path, func(k uint64) error {
		if have && k <= prev {
			t.Fatalf("v3 sidecar not strictly ascending: %d after %d", k, prev)
		}
		prev, have = k, true
		got = append(got, k)
		return nil
	}); err != nil {
		t.Fatalf("stream upgraded sidecar: %v", err)
	}
	return got
}

// expectedSet returns the sorted, de-duplicated key set the migration must
// preserve exactly — the source of truth computed independently of the writer.
func expectedSet(keys []uint64) []uint64 {
	exp := slices.Clone(keys)
	slices.Sort(exp)
	return slices.Compact(exp)
}

// assertUpgradedToV3 checks the on-disk header reflects a sorted v3 sidecar with
// the given key count, then returns the round-tripped keys.
func assertUpgradedToV3(t *testing.T, path string, wantCount uint64) []uint64 {
	t.Helper()
	hdr, err := readSidecarHeader(path)
	if err != nil {
		t.Fatalf("read upgraded header: %v", err)
	}
	if !hdr.sorted() || hdr.formatVersion != sidecarFormatV3 {
		t.Fatalf("not upgraded to sorted v3: formatVersion=%d sorted=%v", hdr.formatVersion, hdr.sorted())
	}
	if hdr.keyCount != wantCount {
		t.Fatalf("header keyCount = %d, want %d", hdr.keyCount, wantCount)
	}
	return readSidecarKeysOrdered(t, path)
}

// TestUpgradeV2ToV3PreservesKeySet drives the core migration transform over a
// range of key distributions and proves the upgraded v3 sidecar holds EXACTLY
// the de-duplicated input key set — no lost keys, no spurious keys (e.g. 0),
// correct ordering — and that re-running the upgrade is a no-op (idempotent).
func TestUpgradeV2ToV3PreservesKeySet(t *testing.T) {
	cases := []struct {
		name string
		keys []uint64
	}{
		{"empty", nil},
		{"single", []uint64{42}},
		{"sorted_no_dups", []uint64{1, 2, 3, 4, 5}},
		{"reverse_no_dups", []uint64{9, 7, 5, 3, 1}},
		{"heavy_dups", []uint64{5, 5, 5, 1, 1, 9, 9, 9, 9, 3}},
		{"all_identical", []uint64{7, 7, 7, 7, 7}},
		{"boundaries", []uint64{0, math.MaxUint64, 0, math.MaxUint64, 12345, 0}},
		{"unsorted_mixed", []uint64{500, 1, 999983, 2, 500, 0, 17, 17}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "sfu_part.txt.zst")
			writeV2Sidecar(t, archive, tc.keys)
			path := sidecarPathForArchive(archive)

			want := expectedSet(tc.keys)

			count, err := upgradeSidecarToV3(context.Background(), path)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			if count != uint64(len(want)) {
				t.Fatalf("returned count = %d, want %d", count, len(want))
			}
			got := assertUpgradedToV3(t, path, uint64(len(want)))
			if !slices.Equal(got, want) {
				t.Fatalf("key set changed by migration:\n got=%v\nwant=%v", got, want)
			}

			// Idempotency: a second upgrade must be a no-op and keep the set.
			count2, err := upgradeSidecarToV3(context.Background(), path)
			if err != nil {
				t.Fatalf("re-upgrade: %v", err)
			}
			if count2 != count {
				t.Fatalf("re-upgrade count = %d, want %d (idempotent)", count2, count)
			}
			if got2 := readSidecarKeysOrdered(t, path); !slices.Equal(got2, want) {
				t.Fatalf("idempotent re-upgrade changed keys:\n got=%v\nwant=%v", got2, want)
			}
		})
	}
}

// TestUpgradeV2ToV3SpillMergePreservesKeySet forces the external spill + k-way
// merge path (by shrinking the in-RAM sort budget) and proves a large v2 sidecar
// with many duplicates migrates to a complete, sorted, de-duplicated v3 set.
func TestUpgradeV2ToV3SpillMergePreservesKeySet(t *testing.T) {
	oldMax := sidecarSortMaxKeys
	sidecarSortMaxKeys = 256 // tiny budget → several spills + a real merge
	defer func() { sidecarSortMaxKeys = oldMax }()

	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_big.txt.zst")

	const n = 5000
	keys := make([]uint64, n)
	for i := range keys {
		// collisions on purpose (mod) so the merge must dedup across runs,
		// in an unsorted order so every spill is independently sorted.
		keys[i] = uint64((i*2654435761 + 7) % 1500)
	}
	writeV2Sidecar(t, archive, keys)
	path := sidecarPathForArchive(archive)
	want := expectedSet(keys)

	count, err := upgradeSidecarToV3(context.Background(), path)
	if err != nil {
		t.Fatalf("upgrade (spill path): %v", err)
	}
	if count != uint64(len(want)) {
		t.Fatalf("count = %d, want %d", count, len(want))
	}
	if got := assertUpgradedToV3(t, path, uint64(len(want))); !slices.Equal(got, want) {
		t.Fatalf("spill/merge migration changed the key set (len got=%d want=%d)", len(got), len(want))
	}
}

// TestRunODScanMigratesMultiPartLibraryPreservesAllKeys exercises a whole
// library of several legacy v2 parts going through the real -od pipeline in one
// run, asserting every part is upgraded to v3 in place with its exact key set
// intact. Covers the parallel migration path, not just the single-sidecar unit.
func TestRunODScanMigratesMultiPartLibraryPreservesAllKeys(t *testing.T) {
	oldMax := sidecarSortMaxKeys
	sidecarSortMaxKeys = 256 // make at least one part take the spill/merge path
	defer func() { sidecarSortMaxKeys = oldMax }()

	dir := t.TempDir()
	tempDir := t.TempDir()
	past := time.Now().Add(-time.Hour)

	bigPart := make([]uint64, 4000)
	for i := range bigPart {
		bigPart[i] = uint64((i*1103515245 + 12345) % 2000)
	}
	parts := map[string][]uint64{
		"sfu_a": {10, 3, 7, 3, 10, 7},                      // small, dup-heavy
		"sfu_b": {0, math.MaxUint64, 5, 0, math.MaxUint64}, // boundary values
		"sfu_c": bigPart,                                   // forces spill/merge
		"sfu_d": {42},                                      // singleton
	}

	want := make(map[string][]uint64, len(parts))
	for stamp, keys := range parts {
		archive := filepath.Join(dir, stamp+".txt.zst")
		if err := os.WriteFile(archive, []byte(stamp+" archive"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeV2Sidecar(t, archive, keys)
		// archive older than the just-written sidecar → classified for in-place
		// upgrade (not stale regen).
		if err := os.Chtimes(archive, past, past); err != nil {
			t.Fatal(err)
		}
		want[stamp] = expectedSet(keys)
	}

	res, err := runODScanSync(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &odMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesUpgraded != len(parts) {
		t.Fatalf("ArchivesUpgraded = %d, want %d", res.ArchivesUpgraded, len(parts))
	}

	for stamp, expKeys := range want {
		archive := filepath.Join(dir, stamp+".txt.zst")
		path := sidecarPathForArchive(archive)
		got := assertUpgradedToV3(t, path, uint64(len(expKeys)))
		if !slices.Equal(got, expKeys) {
			t.Fatalf("part %s key set changed by library migration (len got=%d want=%d)", stamp, len(got), len(expKeys))
		}
	}
}

// TestBucketKeysFailsLoudOnTruncatedSidecar guards the fix that a short read of
// a sidecar bucket is a hard error rather than a silent zero-fill (which would
// inject a spurious key 0 into the dest set). Simulates the file being
// truncated out from under an already-open reader (post-validation TOCTOU).
func TestBucketKeysFailsLoudOnTruncatedSidecar(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_trunc.txt.zst")
	writeSidecarKeysForTest(t, archive, []uint64{3, 1, 4, 1, 5, 9, 2, 6, 8, 7})
	path := sidecarPathForArchive(archive)

	sr, err := openSidecarReader(path) // header validates body size here
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer sr.close()

	// Lop off the last key's bytes after the reader cached keyCount: the bulk
	// read now spans past EOF.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, fi.Size()-sidecarKeyBytes); err != nil {
		t.Fatal(err)
	}

	// Single bucket → reads the whole body in one ReadAt → short read.
	if _, err := sr.bucketKeys(0, 1); err == nil {
		t.Fatal("bucketKeys silently tolerated a truncated sidecar (want a hard error)")
	}
}
