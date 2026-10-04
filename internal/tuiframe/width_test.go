package tuiframe

import (
	"strings"
	"testing"
)

// CJK characters occupy two terminal cells; VisibleWidth must count cells, not runes.
func TestVisibleWidthCountsCells(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"café", 4},
		{"你好", 4},
		{"a你b", 4},
		{"👨‍👩‍👧", 2},   // ZWJ family: one grapheme cluster, 2 cells
		{"e\u0301", 1}, // combining accent: one grapheme, 1 cell
		{"\033[31mab\033[0m", 2},
		{"\033[31m你\033[0m好", 4},
	}
	for _, tc := range cases {
		if got := VisibleWidth(tc.in); got != tc.want {
			t.Errorf("VisibleWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTruncateRightRespectsBudget(t *testing.T) {
	if got := TruncateRight("abcdef", 4); got != "abcd" {
		t.Errorf("TruncateRight ASCII = %q, want %q", got, "abcd")
	}
	// Wide chars: budget 4 holds "ab你" (1+1+2), the second 你 doesn't fit.
	if got := TruncateRight("ab你你", 4); got != "ab你" {
		t.Errorf("TruncateRight wide = %q, want %q", got, "ab你")
	}
	if got := TruncateRight("abcdef", 0); got != "" {
		t.Errorf("TruncateRight max=0 = %q, want empty", got)
	}
	if got := TruncateRight("abcdef", -2); got != "" {
		t.Errorf("TruncateRight max<0 = %q, want empty", got)
	}
}

func TestTruncateRightPreservesANSI(t *testing.T) {
	s := "\033[31mred\033[0mplain"
	got := TruncateRight(s, 4)
	if VisibleWidth(got) != 4 {
		t.Fatalf("width = %d, want 4 (%q)", VisibleWidth(got), got)
	}
	if !strings.Contains(got, "\033[31m") {
		t.Fatalf("ANSI color sequence dropped: %q", got)
	}
	if strings.Contains(got, "plain") {
		t.Fatalf("tail not truncated: %q", got)
	}
}

func TestTruncateRightNeverSplitsGrapheme(t *testing.T) {
	// The family emoji is one 2-cell cluster; budget 1 must not split it —
	// it must return "" (or the empty prefix) rather than half an emoji.
	if got := TruncateRight("👨‍👩‍👧ab", 1); VisibleWidth(got) > 1 {
		t.Errorf("TruncateRight split/overflowed grapheme: %q width %d", got, VisibleWidth(got))
	}
}

func TestTruncateLeftKeepsTailAndEllipsis(t *testing.T) {
	if got := TruncateLeft("abcdefghij", 4); got != "…hij" {
		t.Errorf("TruncateLeft ASCII = %q, want %q", got, "…hij")
	}
	if got := TruncateLeft("short", 8); got != "short" {
		t.Errorf("fitting string must be unchanged: %q", got)
	}
	if got := TruncateLeft("abc", 0); got != "" {
		t.Errorf("max=0 = %q, want empty", got)
	}
}

func TestTruncateLeftWideAndGraphemes(t *testing.T) {
	// 7 cells: a(1) 你(2) b(1) 你(2) c(1). Budget 5 keeps the rightmost 4
	// cells plus the ellipsis.
	if got := TruncateLeft("a你b你c", 5); got != "…b你c" {
		t.Errorf("TruncateLeft wide = %q, want %q", got, "…b你c")
	}
	got := TruncateLeft(strings.Repeat("你", 10)+"tail", 6)
	if VisibleWidth(got) != 6 {
		t.Fatalf("width = %d, want 6 (%q)", VisibleWidth(got), got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("expected ellipsis prefix: %q", got)
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("mojibake in %q", got)
	}
	// Grapheme cluster must never be split even when it straddles the cut.
	fam := "👨‍👩‍👧" // 2-cell cluster
	got = TruncateLeft("ab"+fam+"cd", 5)
	if !utf8ValidNoSplit(got, fam) {
		t.Errorf("grapheme cluster split or mangled: %q", got)
	}
}

func utf8ValidNoSplit(s, cluster string) bool {
	return strings.Contains(s, cluster) || strings.Contains(s, "…"+cluster) || strings.HasSuffix(s, cluster)
}

func TestPadOrTrimExactCells(t *testing.T) {
	if got := PadOrTrim("ab", 5); got != "ab   " {
		t.Errorf("pad = %q, want %q", got, "ab   ")
	}
	if got := PadOrTrim("你好ab", 5); got != "你好a" {
		t.Errorf("trim wide = %q, want %q", got, "你好a")
	}
	if got := PadOrTrim("exact", 5); got != "exact" {
		t.Errorf("exact = %q", got)
	}
	if got := PadOrTrim("x", 0); got != "" {
		t.Errorf("max=0 = %q, want empty", got)
	}
}

func TestPadOrTrimPreservesANSI(t *testing.T) {
	styled := "\033[32mok\033[0m"
	if got := PadOrTrim(styled, 6); got != styled+"    " {
		t.Errorf("pad styled = %q", got)
	}
	if got := PadOrTrim(styled, 1); VisibleWidth(got) != 1 || !strings.Contains(got, "o") {
		t.Errorf("trim styled = %q, want one cell keeping the character", got)
	}
}

func TestPadOrTrimHonorsBudgetsBelow8(t *testing.T) {
	for _, w := range []int{1, 2, 3, 4, 7} {
		if got := PadOrTrim(strings.Repeat("x", 30), w); VisibleWidth(got) != w {
			t.Errorf("PadOrTrim width=%d: got width %d (%q)", w, VisibleWidth(got), got)
		}
	}
}

func TestTruncateLeftMaxOneIsEllipsis(t *testing.T) {
	if got := TruncateLeft("abcdef", 1); got != "…" {
		t.Errorf("TruncateLeft max=1 = %q, want %q", got, "…")
	}
}
