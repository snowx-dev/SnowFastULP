package main

import (
	"slices"
)

// sorted []uint64 + binary search. ~8 B/key vs ~24-32 B for a map, fits
// 1.2M-key buckets in L2 so lookups are roughly map-fast in practice.
type sortedUint64Set struct {
	keys []uint64
}

// adoptSorted takes ownership of keys that are ALREADY sorted ascending and
// deduplicated (e.g. the output of a k-way merge) — the only way production
// populates the set.
func (s *sortedUint64Set) adoptSorted(keys []uint64) { s.keys = keys }

// O(log n), safe for concurrent readers once the set is populated
func (s *sortedUint64Set) Contains(k uint64) bool {
	_, ok := slices.BinarySearch(s.keys, k)
	return ok
}

func (s *sortedUint64Set) Len() int { return len(s.keys) }
