package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
)

func TestRenderFinalSummaryOutputFullPath(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(1)
	path := filepath.Join(t.TempDir(), strings.Repeat("nest/", 20)+"hits/gleeden.txt")
	joined := strings.Join(renderFinalSummary(time.Now(), m, path, "", nil), "\n")
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
	joined := strings.Join(renderFinalSummary(time.Now(), m, rel, "", nil), "\n")
	if !strings.Contains(collapseRenderedText(joined), want) {
		t.Fatalf("missing resolved output path %q in:\n%s", want, joined)
	}
}

func TestRenderFinalSummaryUpdateNoticeFooter(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(1)
	notice := &selfupdate.Notice{Latest: "0.2.0", Command: "sfs update"}
	joined := strings.Join(renderFinalSummary(time.Now(), m, "", "", notice), "\n")
	for _, want := range []string{"Update available", "v0.2.0", "sfs update", "sfs is open-source", "https://snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in summary footer:\n%s", want, joined)
		}
	}
}

func TestRenderFinalSummaryNoNoticeUsesPlainFooter(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(1)
	joined := strings.Join(renderFinalSummary(time.Now(), m, "", "", nil), "\n")
	if strings.Contains(joined, "Update available") {
		t.Fatalf("nil notice should not show update line:\n%s", joined)
	}
	for _, want := range []string{"sfs is open-source", "https://snowx.dev"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in footer:\n%s", want, joined)
		}
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
