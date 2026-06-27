package ulpengine

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// Build sorts+dedups and adopts keys. Test-only: production always feeds the set
// pre-sorted keys via adoptSorted (the k-way merge output), so this convenience
// lives with the tests that exercise the set directly.
func (s *sortedUint64Set) Build(keys []uint64) {
	slices.Sort(keys)
	s.adoptSorted(slices.Compact(keys))
}

func TestSortedUint64SetBasic(t *testing.T) {
	var s sortedUint64Set
	s.Build([]uint64{50, 10, 30, 20, 40})
	for _, k := range []uint64{10, 20, 30, 40, 50} {
		if !s.Contains(k) {
			t.Errorf("Contains(%d) = false, want true", k)
		}
	}
	for _, k := range []uint64{0, 1, 5, 15, 25, 35, 45, 100} {
		if s.Contains(k) {
			t.Errorf("Contains(%d) = true, want false", k)
		}
	}
	if s.Len() != 5 {
		t.Errorf("Len = %d, want 5", s.Len())
	}
}

// concatenated sidecars can legitimately repeat hashes. at 5B keys
// 5% dup = 250M wasted bytes w/o compaction
func TestSortedUint64SetCompactsDuplicates(t *testing.T) {
	var s sortedUint64Set
	s.Build([]uint64{7, 7, 7, 7, 7})
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1 after compaction", s.Len())
	}
	if !s.Contains(7) {
		t.Error("Contains(7) = false after compaction")
	}
}

// phase-2 hot path makes empty sets for no-dest-key buckets, no panics
func TestSortedUint64SetEmpty(t *testing.T) {
	var s sortedUint64Set
	if s.Contains(42) {
		t.Error("zero-value set should not Contains anything")
	}
	if s.Len() != 0 {
		t.Errorf("zero-value Len = %d, want 0", s.Len())
	}
	s.Build(nil)
	if s.Len() != 0 {
		t.Errorf("Build(nil) Len = %d, want 0", s.Len())
	}
}

// 100k keys, exercises bsearch beyond L1. catches off-by-one or
// comparator-direction regressions
func TestSortedUint64SetLarge(t *testing.T) {
	const n = 100_000
	r := rand.New(rand.NewPCG(1, 2))
	keys := make([]uint64, n)
	for i := range keys {
		keys[i] = r.Uint64()
	}
	// snapshot truth before Build dedupes/reorders
	truth := make(map[uint64]struct{}, n)
	for _, k := range keys {
		truth[k] = struct{}{}
	}

	var s sortedUint64Set
	s.Build(keys)

	if s.Len() != len(truth) {
		t.Errorf("Len = %d, want %d (after dedup)", s.Len(), len(truth))
	}
	for k := range truth {
		if !s.Contains(k) {
			t.Fatalf("Contains(%d) = false for inserted key", k)
		}
	}
	// 1000 fresh randoms, collision w/ truth negligible, off-by-one in
	// bsearch would flip far more than ~0
	r2 := rand.New(rand.NewPCG(3, 4))
	falseHits := 0
	for i := 0; i < 1000; i++ {
		k := r2.Uint64()
		if _, present := truth[k]; present {
			continue
		}
		if s.Contains(k) {
			falseHits++
		}
	}
	if falseHits != 0 {
		t.Errorf("%d false hits on 1000 fresh probes", falseHits)
	}
}
