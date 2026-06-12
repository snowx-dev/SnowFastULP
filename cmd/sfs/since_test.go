package main

import (
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
