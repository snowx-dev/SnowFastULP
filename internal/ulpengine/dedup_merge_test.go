package ulpengine

import (
	"slices"
	"testing"
)

func TestSortCompactUnique(t *testing.T) {
	cases := []struct {
		name string
		in   []uint64
		want []uint64
	}{
		{"empty", nil, nil},
		{"single", []uint64{7}, []uint64{7}},
		{"sorted unique", []uint64{1, 4, 9}, []uint64{1, 4, 9}},
		{"unsorted with dups", []uint64{3, 1, 4, 1, 5, 9, 2, 6, 5, 3}, []uint64{1, 2, 3, 4, 5, 6, 9}},
		{"all same", []uint64{5, 5, 5, 5}, []uint64{5}},
		{"reverse", []uint64{9, 5, 1}, []uint64{1, 5, 9}},
	}
	for _, c := range cases {
		got := sortCompactUnique(c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		for i := 1; i < len(got); i++ {
			if got[i] <= got[i-1] {
				t.Errorf("%s: not strictly ascending at %d: %v", c.name, i, got)
			}
		}
	}
}

// the returned slice must alias the input backing array: the gather's memory
// contract is ONE 8B/key allocation, no second merged output.
func TestSortCompactUniqueInPlace(t *testing.T) {
	in := []uint64{5, 1, 5, 9, 1, 9, 3, 3}
	out := sortCompactUnique(in)
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4", len(out))
	}
	if !slices.Equal(out, []uint64{1, 3, 5, 9}) {
		t.Fatalf("got %v, want [1 3 5 9]", out)
	}
	if cap(out) != cap(in) {
		t.Fatalf("compact reallocated: cap(out) = %d, want input cap %d", cap(out), cap(in))
	}
	// out must share storage with in
	out[0] = 42
	if in[0] != 42 {
		t.Fatalf("out is not an alias of the input backing array")
	}
}
