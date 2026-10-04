package search_test

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// per-worker single-slot fileCache must close prev archive before next.
// process holds <=2 archive fds at once. non-linux = correctness smoke
func TestSearchEvictsFileCacheOnArchiveTransition(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fd-count check uses /proc, correctness exercised everywhere")
	}
	const N = 8
	dir := t.TempDir()
	archives := make([]string, N)
	sidecars := map[string]*index.Sidecar{}
	ord := map[string]int{}
	for i := 0; i < N; i++ {
		p := filepath.Join(dir, "a"+itoaPad(i)+".zst")
		writeBytesZST(t, p, []byte("alpha\nbeta\nneedle\n"))
		sc, err := index.Build(context.Background(), p, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		archives[i] = p
		sidecars[p] = sc
		ord[p] = i
	}

	var maxOpen atomic.Int32
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			// best-effort sample, not strict
			n := countProcArchiveFDs(dir)
			for {
				cur := maxOpen.Load()
				if int32(n) <= cur || maxOpen.CompareAndSwap(cur, int32(n)) {
					break
				}
			}
		}
	}()

	hitCh := make(chan search.Hit, 64)
	err := search.Run(search.Config{
		Ctx:        context.Background(),
		Pattern:    []byte("needle"),
		Workers:    1,
		Archives:   archives,
		Sidecars:   sidecars,
		ArchiveOrd: ord,
		Hits:       hitCh,
	})
	close(stop)
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	for range hitCh {
		count++
	}
	if count != N {
		t.Fatalf("hits = %d, want %d", count, N)
	}
	// 1 worker holds <=2 archive fds simultaneously (close+open),
	// slack for sampling skew
	if got := maxOpen.Load(); got > 3 {
		t.Fatalf("max simultaneous archive fds = %d, want <=3", got)
	}
}

func itoaPad(i int) string {
	if i < 10 {
		return "0" + strconv.Itoa(i)
	}
	return strconv.Itoa(i)
}

func countProcArchiveFDs(dir string) int { return procArchiveFDCount(dir) }
