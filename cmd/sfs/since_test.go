package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"7d", 7 * 24 * time.Hour},
		{"12h", 12 * time.Hour},
		{"90m", 90 * time.Minute},
		{"1d6h", 24*time.Hour + 6*time.Hour},
		{" 2d ", 2 * 24 * time.Hour},
		// MaxInt64 nanoseconds is ~106751 days; the largest valid day count
		// plus a remainder that still fits must parse exactly.
		{"106751d", 106751 * 24 * time.Hour},
		{"106751d20h", 106751*24*time.Hour + 20*time.Hour},
	}
	for _, c := range cases {
		got, err := parseSince(c.in)
		if err != nil {
			t.Errorf("parseSince(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSince(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseSinceInvalid(t *testing.T) {
	for _, in := range []string{"7", "abc", "d", "0d", "-3h", "7x"} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) = nil error, want error", in)
		}
	}
}

// TestParseSinceOverflowRejected pins the L-01 fix: day counts whose
// nanosecond product no longer fits in time.Duration (and combos whose sum
// overflows) must be rejected, not wrap into a small positive duration that
// silently searches the wrong window (213504d used to wrap to ~25 minutes).
func TestParseSinceOverflowRejected(t *testing.T) {
	for _, in := range []string{
		"213504d",         // product wraps to ~25m without the bound
		"106752d",         // just past the day bound
		"99999999999d",    // far past
		"-213504d",        // negative overflow past the day bound
		"106751d2562047h", // day product + ParseDuration max overflows the sum
	} {
		if _, err := parseSince(in); err == nil {
			t.Errorf("parseSince(%q) = nil error, want out-of-range error", in)
		} else if !strings.Contains(err.Error(), "out of range") {
			t.Errorf("parseSince(%q) err = %v, want out-of-range message", in, err)
		}
	}
}
