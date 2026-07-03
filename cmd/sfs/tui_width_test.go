package main

import (
	"strings"
	"testing"
)

func TestTuiVisibleWidthStyled(t *testing.T) {
	s := labelStyle.Render("Archives  ") + countStyle.Render("0") + mutedStyle.Render(" / ") + countStyle.Render("31")
	if got := tuiVisibleWidth(s); got < 10 {
		t.Fatalf("visible width too small: %d for %q", got, s)
	}
}

func TestPadOrTrimStyled(t *testing.T) {
	inner := labelStyle.Render("Index     ") + byteStyle.Render("1.2 GB") + mutedStyle.Render(" / ") + byteStyle.Render("110.0 GB")
	got := padOrTrim(inner, 40)
	if tuiVisibleWidth(got) != 40 {
		t.Fatalf("visible width = %d, want 40", tuiVisibleWidth(got))
	}
}

func TestTrimToDisplayWidthPreservesStyle(t *testing.T) {
	long := strings.Repeat("x", 120)
	styled := hitStyle.Render(long)
	got := trimToDisplayWidth(styled, 20)
	if tuiVisibleWidth(got) > 20 {
		t.Fatalf("visible width = %d, want <= 20", tuiVisibleWidth(got))
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected ellipsis in %q", got)
	}
}

func TestTuiVisibleWidthUnicode(t *testing.T) {
	if tuiVisibleWidth("café") != 4 {
		t.Fatalf("café width = %d", tuiVisibleWidth("café"))
	}
}

func TestContentWidthBalanced(t *testing.T) {
	if got := contentWidth(80); got != 72 {
		t.Fatalf("contentWidth(80) = %d, want 72", got)
	}
	if got := boxInnerWidth(80); got != 66 {
		t.Fatalf("boxInnerWidth(80) = %d, want 66", got)
	}
}
