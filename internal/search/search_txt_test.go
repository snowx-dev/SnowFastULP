package search_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestRunTxtFindsMatches(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.txt")
	p2 := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(p1, []byte("alpha\nneedle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("no match\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := []string{p1, p2}
	ord := map[string]int{p1: 0, p2: 1}
	hitCh := make(chan search.Hit, 8)
	ctx := context.Background()

	err := search.RunTxt(search.TxtConfig{
		Ctx:        ctx,
		Pattern:    []byte("needle"),
		Workers:    2,
		Files:      files,
		ArchiveOrd: ord,
		Hits:       hitCh,
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}

	var lines []string
	for h := range hitCh {
		lines = append(lines, h.Line)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "needle") {
		t.Fatalf("hits = %v", lines)
	}
}

func TestRunTxtCancel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	var b strings.Builder
	for i := 0; i < 10000; i++ {
		b.WriteString("line without match\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	hitCh := make(chan search.Hit, 1)
	err := search.RunTxt(search.TxtConfig{
		Ctx:        ctx,
		Pattern:    []byte("zzz"),
		Workers:    1,
		Files:      []string{p},
		ArchiveOrd: map[string]int{p: 0},
		Hits:       hitCh,
	})
	close(hitCh)
	if err == nil {
		t.Fatal("expected context error")
	}
}
