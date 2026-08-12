package tuistat

import "testing"

func TestShareParenOmitWhenDenomInvalid(t *testing.T) {
	if got := ShareParen(5, 0); got != "" {
		t.Fatalf("of=0: got %q, want empty", got)
	}
	if got := ShareParen(5, -1); got != "" {
		t.Fatalf("of<0: got %q, want empty", got)
	}
}

func TestShareParenOmitWhenWhole(t *testing.T) {
	if got := ShareParen(10, 10); got != "" {
		t.Fatalf("n==of: got %q, want empty (no 100%%)", got)
	}
}

func TestShareParenOmitImpossibleRatios(t *testing.T) {
	if got := ShareParen(-1, 10); got != "" {
		t.Fatalf("n<0: got %q, want empty", got)
	}
	if got := ShareParen(11, 10); got != "" {
		t.Fatalf("n>of: got %q, want empty", got)
	}
}

func TestShareParenOmitWhenRoundsTo100(t *testing.T) {
	// 999995/1e6 formats as 100.0 with %.1f but n != of.
	if got := ShareParen(999_995, 1_000_000); got != "" {
		t.Fatalf("near-whole: got %q, want empty (no rounded 100%%)", got)
	}
}

func TestShareParenOmitWhenRoundsTo0(t *testing.T) {
	if got := ShareParen(5, 1_000_000); got != "" {
		t.Fatalf("near-zero: got %q, want empty (no rounded 0%%)", got)
	}
	if got := ShareParen(0, 10); got != "" {
		t.Fatalf("exact zero: got %q, want empty", got)
	}
}

func TestShareParenFormatsOneDecimal(t *testing.T) {
	cases := []struct {
		n, of int64
		want  string
	}{
		{8, 10, " (80.0%)"},
		{1, 3, " (33.3%)"},
		{2, 10, " (20.0%)"},
		{999_000, 1_000_000, " (99.9%)"}, // below the %.1f round-up-to-100 band
	}
	for _, tc := range cases {
		if got := ShareParen(tc.n, tc.of); got != tc.want {
			t.Fatalf("ShareParen(%d, %d) = %q, want %q", tc.n, tc.of, got, tc.want)
		}
	}
}
