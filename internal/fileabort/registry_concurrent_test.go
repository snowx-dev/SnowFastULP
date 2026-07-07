package fileabort_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

// N goroutines register fresh files while another sweeps w/ CloseAll.
// -race covers the lock, this adds the missing concurrent shape.
// post: no panics, leaks, or leftover fds. linux cross-checks /proc/self/fd
func TestRegistryConcurrentRegisterAndCloseAll(t *testing.T) {
	dir := t.TempDir()
	const goroutines = 32
	const filesPerG = 16

	baseFD := countOpenFDs(t)

	r := &fileabort.Registry{}

	var wg sync.WaitGroup
	closerDone := make(chan struct{})

	// producers: open + register, keep unregisters for happy-path drop
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			unregs := make([]func(), 0, filesPerG)
			files := make([]*os.File, 0, filesPerG)
			for i := 0; i < filesPerG; i++ {
				path := filepath.Join(dir, "g"+strconv.Itoa(g)+"_"+strconv.Itoa(i)+".dat")
				f, err := os.Create(path)
				if err != nil {
					t.Errorf("open: %v", err)
					return
				}
				files = append(files, f)
				unregs = append(unregs, r.Register(f))
			}
			// half unreg+close, half lean on CloseAll. mirrors prod where
			// some workers exit normal, others die to ctx cancel
			if g%2 == 0 {
				for i, u := range unregs {
					u()
					_ = files[i].Close()
				}
			}
		}(g)
	}

	// closer races producers, CloseAll must be safe to call repeatedly
	go func() {
		defer close(closerDone)
		for i := 0; i < 8; i++ {
			r.CloseAll()
			runtime.Gosched()
		}
	}()

	wg.Wait()
	<-closerDone
	r.CloseAll() // final sweep, catches anything the closer raced past

	if runtime.GOOS == "linux" {
		// slack for transient runner fds, only flag big leaks
		now := countOpenFDs(t)
		if now > baseFD+8 {
			t.Errorf("FD count grew from %d → %d (suspected leak)", baseFD, now)
		}
	}
}
