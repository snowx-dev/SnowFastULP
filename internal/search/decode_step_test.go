package search

import "testing"

// DecodeStep knob plumbed thru Config + clamped to safe range,
// direct call avoids spinning up a zstd archive for a guard test
func TestResolveDecodeStep(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, defaultDecodeStep},
		{-1, defaultDecodeStep},
		{1, minDecodeStep},
		{minDecodeStep - 1, minDecodeStep},
		{minDecodeStep, minDecodeStep},
		{256 << 10, 256 << 10},
		{outWin, outWin},
		{outWin + 1, outWin},
		{1 << 30, outWin},
	}
	for _, c := range cases {
		if got := resolveDecodeStep(c.in); got != c.want {
			t.Errorf("resolveDecodeStep(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
