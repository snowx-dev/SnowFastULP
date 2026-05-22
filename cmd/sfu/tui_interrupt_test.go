package main

import (
	"strings"
	"testing"
)

func TestRenderLinesRowInlineWhenItFits(t *testing.T) {
	stats := []lineStat{
		{"total", "100", countStyle},
		{"accepted", "90", acceptStyle},
		{"rejected", "10", mutedStyle},
	}
	inline := "Lines    100 total · 90 accepted · 10 rejected"
	got := renderLinesRow(inline, stats, 200) // generous width
	if len(got) != 1 {
		t.Fatalf("expected 1 row when fits, got %d: %v", len(got), got)
	}
	if got[0] != inline {
		t.Errorf("expected inline pass-through, got %q", got[0])
	}
}

func TestRenderLinesRowStacksWhenTooWide(t *testing.T) {
	stats := []lineStat{
		{"total", "12,345,678", countStyle},
		{"accepted", "11,000,000", acceptStyle},
		{"rejected", "1,345,678", mutedStyle},
	}
	inline := strings.Repeat("X", 200)
	got := renderLinesRow(inline, stats, 60)
	if len(got) != 3 {
		t.Fatalf("expected 3 stacked rows, got %d: %v", len(got), got)
	}
	// each row has its number, only first has "Lines" label
	if !strings.Contains(got[0], "12,345,678") {
		t.Errorf("row 0 missing total: %q", got[0])
	}
	if !strings.Contains(got[0], "Lines") {
		t.Errorf("row 0 missing 'Lines' header: %q", got[0])
	}
	if !strings.Contains(got[1], "11,000,000") {
		t.Errorf("row 1 missing accepted: %q", got[1])
	}
	if strings.Contains(got[1], "Lines") {
		t.Errorf("row 1 should not repeat 'Lines' header: %q", got[1])
	}
	if !strings.Contains(got[2], "1,345,678") {
		t.Errorf("row 2 missing rejected: %q", got[2])
	}
	// numeric values right-padded to widest, 10 chars w/ commas
	for i, row := range got {
		if tuiVisibleWidth(row) < statLabelColWidth+len("accepted")+2+10 {
			t.Errorf("row %d narrower than expected alignment: visible width %d, content %q",
				i, tuiVisibleWidth(row), row)
		}
	}
}

func TestStatLabelPadsToColumn(t *testing.T) {
	s := statLabel("Lines")
	if got := tuiVisibleWidth(s); got != statLabelColWidth {
		t.Errorf("statLabel(\"Lines\") visible width = %d, want %d", got, statLabelColWidth)
	}
}

func TestRenderInterruptLinesContainsHint(t *testing.T) {
	lines := renderInterruptLines(0, 80)
	if len(lines) == 0 {
		t.Fatal("expected non-empty interrupt frame")
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{"INTERRUPTED", "Ctrl+C", "cleanup", "Flushing"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected interrupt frame to contain %q, got:\n%s", want, all)
		}
	}
}
