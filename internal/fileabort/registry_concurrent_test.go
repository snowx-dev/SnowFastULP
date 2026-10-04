package fileabort_test

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestRegistryContentionSweepClosesEveryDescriptor: a single abort sweep
// racing a registration storm must leave ZERO open handles — any Register
// landing after the sweep is closed immediately by Register itself, so no
// descriptor can outlive the sweep even without a follow-up CloseAll.
func TestRegistryContentionSweepClosesEveryDescriptor(t *testing.T) {
	dir := t.TempDir()
	r := &fileabort.Registry{}
	const producers = 8

	var mu sync.Mutex
	var allFiles []*os.File
	var nextName atomic.Int64

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				path := filepath.Join(dir, "p"+strconv.FormatInt(nextName.Add(1), 10)+".dat")
				f, err := os.Create(path)
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				if _, err := f.WriteString("x"); err != nil {
					t.Errorf("write: %v", err)
					return
				}
				mu.Lock()
				allFiles = append(allFiles, f)
				mu.Unlock()
				r.Register(f)
			}
		}()
	}

	// let the producers pile up registrations, then exactly ONE sweep
	time.Sleep(10 * time.Millisecond)
	r.CloseAll()
	close(stop)
	wg.Wait()

	// every descriptor ever registered must be closed now: the sweep closed
	// pre-snapshot ones, Register closed post-sweep ones. An open, readable
	// handle here means it escaped the abort sweep.
	mu.Lock()
	defer mu.Unlock()
	for i, f := range allFiles {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if n, rerr := f.Read(make([]byte, 1)); rerr == nil && n == 1 {
				t.Fatalf("file #%d (%s) escaped the abort sweep", i, f.Name())
			}
		}
	}
}
