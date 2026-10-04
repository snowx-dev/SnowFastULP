package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// Two racers composing sfu's -debug artifact name — stem, RunStamp, and the
// shared allocator — must each get their own file (F11): the allocator's
// exclusive create turns a same-name collision into the _2 suffix instead of
// two O_TRUNC opens on one prospective path.
func TestConcurrentDebugArtifactCreation(t *testing.T) {
	dir := t.TempDir()
	stamp := ulpengine.RunStamp(time.Date(2020, 1, 2, 15, 4, 5, 0, time.UTC), "test01")
	paths := make([]string, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, p, err := ulpengine.CreateArtifactFile(dir, "sfu-debug-"+stamp, ".log", 0o600)
			if err != nil {
				t.Error(err)
				return
			}
			paths[i] = p
			_ = f.Close()
		}()
	}
	wg.Wait()

	want := map[string]bool{
		filepath.Join(dir, "sfu-debug-"+stamp+".log"):   true,
		filepath.Join(dir, "sfu-debug-"+stamp+"_2.log"): true,
	}
	for _, p := range paths {
		if p == "" {
			t.Fatal("a racer returned no path")
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if !want[p] {
			t.Fatalf("unexpected path %q (want sfu-debug-%s.log or its _2 suffix)", p, stamp)
		}
		delete(want, p)
	}
	if len(want) != 0 {
		t.Fatalf("missing distinct path; two racers got %v", paths)
	}
}
