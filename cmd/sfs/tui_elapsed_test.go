package main

import (
	"testing"
	"time"
)

// Sub-second runs must not render as a flat "0s" — the -stats Elapsed row is
// how users sanity-check a fast search, so one decimal is kept below 10s.
func TestFormatElapsedSubSecondShowsDecimal(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0.0s"},
		{400 * time.Millisecond, "0.4s"},
		{9_940 * time.Millisecond, "9.9s"},
		{10 * time.Second, "10s"},
		{42 * time.Second, "42s"},
		{90 * time.Second, "1m30s"},
	}
	for _, c := range cases {
		if got := formatElapsed(c.in); got != c.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
