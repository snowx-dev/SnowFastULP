package main

import (
	"slices"
)

// sorted []uint64 + binary search. ~8 B/key vs ~24-32 B for a map, fits
// 1.2M-key buckets in L2 so lookups are roughly map-fast in practice.
type sortedUint64Set struct {
	keys []uint64
}

// sorts and dedups in place, takes ownership of keys
func (s *sortedUint64Set) Build(keys []uint64) {
	slices.Sort(keys)
	s.keys = slices.Compact(keys)
}

// adoptSorted takes ownership of keys that are ALREADY sorted ascending and
// deduplicated (e.g. the output of a k-way merge), skipping the Build sort.
func (s *sortedUint64Set) adoptSorted(keys []uint64) { s.keys = keys }

// O(log n), safe for concurrent readers post-Build
func (s *sortedUint64Set) Contains(k uint64) bool {
	_, ok := slices.BinarySearch(s.keys, k)
	return ok
}

func (s *sortedUint64Set) Len() int { return len(s.keys) }
