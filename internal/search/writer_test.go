package search_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestWriterWriteHitLineOnly(t *testing.T) {
	var buf bytes.Buffer
	w := search.NewWriter(&buf, false)
	err := w.WriteHit(search.Hit{
		Archive: "/data/part1.txt.zst",
		Line:    "user@example.com:password",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "user@example.com:password" {
		t.Fatalf("got %q, want ULP line only", got)
	}
	if strings.Contains(got, "part1.txt.zst") {
		t.Fatalf("archive path leaked into output: %q", got)
	}
}
