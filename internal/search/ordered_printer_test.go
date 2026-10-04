package search_test

import (
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestOrderedPrinterFlushesSortedWhenArchiveDone(t *testing.T) {
	var lines []string
	p := search.NewOrderedPrinter(func(h search.Hit) error {
		lines = append(lines, h.Line)
		return nil
	})

	if err := p.Add(search.Hit{ArchiveOrd: 0, ChunkID: 2, Offset: 10, Line: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Add(search.Hit{ArchiveOrd: 0, ChunkID: 1, Offset: 5, Line: "a"}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("unexpected early write: %v", lines)
	}

	if err := p.MarkArchiveDone(0); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, ","); got != "a,b" {
		t.Fatalf("after archive done = %q, want a,b", got)
	}
}

func TestOrderedPrinterBlocksLaterArchiveUntilEarlierDone(t *testing.T) {
	var lines []string
	p := search.NewOrderedPrinter(func(h search.Hit) error {
		lines = append(lines, h.Line)
		return nil
	})

	if err := p.Add(search.Hit{ArchiveOrd: 1, Line: "later"}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("unexpected early write: %v", lines)
	}

	if err := p.MarkArchiveDone(0); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("archive 1 should wait for its own completion; got %v", lines)
	}

	if err := p.MarkArchiveDone(1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, ","); got != "later" {
		t.Fatalf("after archive 1 done = %q, want later", got)
	}
}

func TestOrderedPrinterFlushesAfterHitsThenMarkArchiveDone(t *testing.T) {
	var lines []string
	p := search.NewOrderedPrinter(func(h search.Hit) error {
		lines = append(lines, h.Line)
		return nil
	})

	if err := p.Add(search.Hit{ArchiveOrd: 0, Line: "hit"}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("unexpected early write: %v", lines)
	}
	if err := p.MarkArchiveDone(0); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, ","); got != "hit" {
		t.Fatalf("got %q, want hit", got)
	}
}

func TestOrderedPrinterHitlessArchiveDoesNotBlockAfterDone(t *testing.T) {
	var lines []string
	p := search.NewOrderedPrinter(func(h search.Hit) error {
		lines = append(lines, h.Line)
		return nil
	})

	if err := p.Add(search.Hit{ArchiveOrd: 2, Line: "hit"}); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("unexpected early write: %v", lines)
	}

	if err := p.MarkArchiveDone(0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkArchiveDone(1); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("archive 2 should wait for its own completion; got %v", lines)
	}

	if err := p.MarkArchiveDone(2); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lines, ","); got != "hit" {
		t.Fatalf("after archive 2 done = %q, want hit", got)
	}
}
