package main

import (
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestLibrarySizeRowShowsCompressedHeadlineAndRatio(t *testing.T) {
	m := &search.Metrics{}
	m.IndexBytesTotal.Store(10 << 30)    // 10 GiB on disk (compressed)
	m.BytesScannedTotal.Store(400 << 30) // 400 GiB uncompressed
	row := renderLibrarySizeRow(m)

	if !strings.Contains(row, "on disk") {
		t.Fatalf("missing on-disk headline: %q", row)
	}
	if !strings.Contains(row, "uncompressed") || !strings.Contains(row, "×") {
		t.Fatalf("missing discreet uncompressed/ratio: %q", row)
	}
	if !strings.Contains(row, "40×") {
		t.Fatalf("expected ~40x ratio, got: %q", row)
	}
}

// -txt (no compression): ratio suffix is omitted, only the on-disk size shows.
func TestLibrarySizeRowOmitsRatioWhenNoCompression(t *testing.T) {
	m := &search.Metrics{}
	m.IndexBytesTotal.Store(1 << 30)
	m.BytesScannedTotal.Store(1 << 30)
	row := renderLibrarySizeRow(m)
	if strings.Contains(row, "uncompressed") || strings.Contains(row, "×") {
		t.Fatalf("should not show ratio when uncompressed == compressed: %q", row)
	}
}
