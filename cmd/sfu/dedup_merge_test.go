package main

import "testing"

func TestMergeSortedUnique(t *testing.T) {
	cases := []struct {
		name string
		runs [][]uint64
		want []uint64
	}{
		{"empty", nil, nil},
		{"single passthrough", [][]uint64{{1, 4, 9}}, []uint64{1, 4, 9}},
		{"disjoint", [][]uint64{{1, 3}, {2, 4}}, []uint64{1, 2, 3, 4}},
		{"cross-run dups", [][]uint64{{1, 3, 5, 7}, {2, 3, 8}, {}, {5, 9}, {0}}, []uint64{0, 1, 2, 3, 5, 7, 8, 9}},
		{"all same", [][]uint64{{5}, {5}, {5}}, []uint64{5}},
		{"within+cross dups", [][]uint64{{5, 5, 9}, {5, 9}}, []uint64{5, 9}},
	}
	for _, c := range cases {
		total := 0
		for _, r := range c.runs {
			total += len(r)
		}
		got := mergeSortedUnique(c.runs, total)
		if !sliceEqualUint64(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := 1; i < len(got); i++ {
			if got[i] <= got[i-1] {
				t.Errorf("%s: not strictly ascending at %d: %v", c.name, i, got)
			}
		}
	}
}
