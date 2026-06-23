package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// With -o (ordered output), hits from many archives that finish in arbitrary
// order must still be written grouped by archive (in archives[] order) and in
// row order within each archive — and every hit must be present. This guards
// the incremental mid-run flush (archive-done → drain buffered hits →
// MarkArchiveDone): a regression that flushes too early would strand late hits
// (missing lines) or interleave archives (out-of-order lines).
func TestRunOrderedOutputMultiArchiveCompleteAndInOrder(t *testing.T) {
	dir := t.TempDir()
	names := []string{"a", "b", "c", "d", "e"}
	const rowsPer = 60

	var archives []string
	var want []string
	for _, name := range names {
		arch := filepath.Join(dir, name+".zst")
		var buf bytes.Buffer
		for i := 0; i < rowsPer; i++ {
			line := fmt.Sprintf("%s.example.com:r%03d@x:needle", name, i)
			fmt.Fprintln(&buf, line)
			want = append(want, line)
		}
		writeZST(t, arch, buf.Bytes())
		if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		archives = append(archives, arch)
	}

	outPath := filepath.Join(dir, "hits.txt")
	metrics := &search.Metrics{}
	if err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: archives,
		workers:  4, // concurrent → archives complete out of order
		outFile:  outPath,
		silent:   true,
		started:  time.Now(),
		metrics:  metrics,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("emitted %d lines, want %d (incremental flush dropped or duplicated hits)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d out of order:\n got=%q\nwant=%q", i, got[i], want[i])
		}
	}
	if metrics.Hits.Load() != int64(len(want)) {
		t.Fatalf("metrics.Hits = %d, want %d", metrics.Hits.Load(), len(want))
	}
}
