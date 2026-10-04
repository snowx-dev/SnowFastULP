package main

import (
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

func TestHistoryRowShowsCheckedAndSkipped(t *testing.T) {
	lines := renderFinalSummary("out.txt", sflog.ExtractStats{HistoryChecked: 3, HistorySkipped: 2})
	plain := strings.Join(lines, "\n")
	if !strings.Contains(plain, "History") || !strings.Contains(plain, "2 skipped") {
		t.Fatalf("summary missing history row:\n%s", plain)
	}
	// sfu-aligned: the checked count rides the same History row.
	if !strings.Contains(plain, "3 checked") {
		t.Fatalf("summary missing checked count:\n%s", plain)
	}
}

func TestHistorySkippedRowAbsentWhenZero(t *testing.T) {
	lines := renderFinalSummary("out.txt", sflog.ExtractStats{HistoryChecked: 3})
	plain := strings.Join(lines, "\n")
	if strings.Contains(plain, "Skipped") {
		t.Fatalf("zero history-skipped count must omit the Skipped row:\n%s", plain)
	}
}
