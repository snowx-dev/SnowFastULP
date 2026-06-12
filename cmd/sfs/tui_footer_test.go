package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestRenderFinalSummaryOutputFullPath(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(1)
	path := filepath.Join(t.TempDir(), strings.Repeat("nest/", 20)+"hits/gleeden.txt")
	joined := strings.Join(renderFinalSummary(time.Now(), m, path, ""), "\n")
	if !strings.Contains(collapseRenderedText(joined), path) {
		t.Fatalf("missing full output path:\n%s", joined)
	}
	if strings.ContainsRune(joined, '…') {
		t.Fatalf("output path should not be ellipsize in summary:\n%s", joined)
	}
}

func TestRenderFinalSummaryOutputResolvesRelativePath(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(1)
	rel := "hits.txt"
	want, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(renderFinalSummary(time.Now(), m, rel, ""), "\n")
	if !strings.Contains(collapseRenderedText(joined), want) {
		t.Fatalf("missing resolved output path %q in:\n%s", want, joined)
	}
}

func collapseRenderedText(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		if r == '\n' || r == '┃' || r == ' ' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
