package search_test

import (
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// many goroutines Add across random archive ords + MarkArchiveDone.
// -race catches lock misses on pending/archiveDone, asserts catch
// ordering bugs. fills audit gap b/c single-threaded tests gave -race
// no reason to look at internals
func TestOrderedPrinterConcurrentAddAndMark(t *testing.T) {
	const archives = 32
	const hitsPerArchive = 50

	var written []search.Hit
	var writeMu sync.Mutex
	var writeCount int64

	p := search.NewOrderedPrinter(func(h search.Hit) error {
		writeMu.Lock()
		written = append(written, h)
		writeMu.Unlock()
		atomic.AddInt64(&writeCount, 1)
		return nil
	})

	var wg sync.WaitGroup
	for ord := 0; ord < archives; ord++ {
		wg.Add(1)
		go func(ord int) {
			defer wg.Done()
			for i := 0; i < hitsPerArchive; i++ {
				h := search.Hit{
					ArchiveOrd: ord,
					Archive:    "fake.zst",
					ChunkID:    i % 4,
					Offset:     int64(i),
					Line:       "match",
				}
				if err := p.Add(h); err != nil {
					t.Errorf("Add(ord=%d): %v", ord, err)
					return
				}
			}
			if err := p.MarkArchiveDone(ord); err != nil {
				t.Errorf("MarkArchiveDone(ord=%d): %v", ord, err)
			}
		}(ord)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&writeCount); got != archives*hitsPerArchive {
		t.Fatalf("write count = %d, want %d", got, archives*hitsPerArchive)
	}

	// ordering: archive ord strict, then ChunkID then Offset ascending
	for i := 1; i < len(written); i++ {
		prev, cur := written[i-1], written[i]
		if cur.ArchiveOrd < prev.ArchiveOrd {
			t.Fatalf("out-of-order archive ord at %d: prev=%d cur=%d", i, prev.ArchiveOrd, cur.ArchiveOrd)
		}
		if cur.ArchiveOrd == prev.ArchiveOrd {
			if cur.ChunkID < prev.ChunkID || (cur.ChunkID == prev.ChunkID && cur.Offset < prev.Offset) {
				t.Fatalf("out-of-order hit within archive %d at i=%d: prev=(%d,%d) cur=(%d,%d)",
					cur.ArchiveOrd, i, prev.ChunkID, prev.Offset, cur.ChunkID, cur.Offset)
			}
		}
	}

	// every archive ord 0..N-1 appears, ascending, no dup runs
	seen := make([]int, 0, archives)
	last := -1
	for _, h := range written {
		if h.ArchiveOrd != last {
			seen = append(seen, h.ArchiveOrd)
			last = h.ArchiveOrd
		}
	}
	if len(seen) != archives {
		t.Fatalf("saw %d distinct archive ord runs, want %d (seen=%v)", len(seen), archives, seen)
	}
	if !sort.IntsAreSorted(seen) {
		t.Fatalf("archive ord runs not sorted: %v", seen)
	}
}
