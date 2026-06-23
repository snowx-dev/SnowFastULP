package main

import (
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestRenderFullShowsQuery(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	out := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, "facebook.com:"), "\n")
	if !strings.Contains(out, "Query") || !strings.Contains(out, "facebook.com:") {
		t.Fatalf("renderFull missing query row:\n%s", out)
	}
}

func TestRenderFinalSummaryShowsQuery(t *testing.T) {
	m := &search.Metrics{}
	out := strings.Join(renderFinalSummary(time.Now(), m, "", "user@example", nil), "\n")
	if !strings.Contains(out, "Query") || !strings.Contains(out, "user@example") {
		t.Fatalf("renderFinalSummary missing query row:\n%s", out)
	}
}

func TestRenderQueryLineEmptyOmitted(t *testing.T) {
	if got := renderQueryLine("", 80); got != "" {
		t.Fatalf("empty pattern should yield no row, got %q", got)
	}
}

func TestRenderQueryLineMatchAll(t *testing.T) {
	got := renderQueryLine("*", 80)
	if !strings.Contains(got, "* (all lines)") {
		t.Fatalf("match-all query should show (all lines), got %q", got)
	}
}

func TestRenderQueryLineTruncatesLong(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := renderQueryLine(long, 80)
	if tuiVisibleWidth(got) > 80 {
		t.Fatalf("query row width %d exceeds innerW 80", tuiVisibleWidth(got))
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("expected ellipsis in truncated query, got %q", got)
	}
}
