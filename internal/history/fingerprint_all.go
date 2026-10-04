package history

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// FingerprintUnit is one fingerprintable source for FingerprintAll: either a
// single file (Path set, Volumes empty) or a multipart assembly (Label set,
// Volumes holding the part paths). A multipart assembly is always hashed
// internally sequentially — part order defines the identity — but separate
// units (whole assemblies and single files) are distributed across workers
// like any other unit.
type FingerprintUnit struct {
	Path    string
	Label   string
	Volumes []string
}

// AllProgress reports aggregate prehash progress across all workers:
// filesDone/filesTotal units fully hashed and bytesDone/bytesTotal bytes
// hashed. It is called concurrently from worker goroutines, so
// implementations must be safe for concurrent use; throttling belongs to the
// display layer, never here.
type AllProgress func(filesDone, filesTotal int, bytesDone, bytesTotal int64)

// defaultFingerprintWorkers caps the parallel prehash: each worker holds at
// most one open file descriptor and one pooled 1MiB hash buffer at a time, so
// the cap bounds the prehash at 8 fds and 8MiB of pooled buffers.
func defaultFingerprintWorkers() int {
	return min(runtime.GOMAXPROCS(0), 8)
}

// FingerprintAll fingerprints every unit with bounded parallelism and returns
// the candidates indexed by input order, whatever the completion order —
// result slots are written by index, never appended, so candidate order (and
// therefore downstream PendingPaths order and run behavior) is deterministic.
//
// Worker count: workers <= 0 selects min(GOMAXPROCS, 8); the value is also
// capped at len(units).
//
// The input list is stat'ed up front in input order so progress can show real
// totals from the first frame; a stat failure surfaces with the same error a
// sequential pass would report. The first failing unit cancels all other
// workers through the context and its error is returned; canceled workers
// release their fileabort registrations via the normal
// FingerprintFile/FingerprintMultipart defers. On error the candidate list is
// not meaningful.
func FingerprintAll(ctx context.Context, units []FingerprintUnit, workers int, progress AllProgress) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	candidates := make([]Candidate, len(units))
	if len(units) == 0 {
		return candidates, nil
	}
	if workers <= 0 {
		workers = defaultFingerprintWorkers()
	}
	workers = min(workers, len(units))

	sizes := make([]int64, len(units))
	var total int64
	for i, u := range units {
		size, err := statUnit(u)
		if err != nil {
			return nil, err
		}
		sizes[i] = size
		total += size
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		next      atomic.Int64
		doneBytes atomic.Int64
		filesDone atomic.Int64
		firstErr  error
		errOnce   sync.Once
		wg        sync.WaitGroup
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(units) {
					return
				}
				if err := ctx.Err(); err != nil {
					fail(err)
					return
				}
				u := units[i]
				var last int64
				unitProgress := func(_ string, done, _ int64) {
					aggregate := doneBytes.Add(done - last)
					last = done
					if progress != nil {
						progress(int(filesDone.Load()), len(units), aggregate, total)
					}
				}
				var (
					c   Candidate
					err error
				)
				if len(u.Volumes) > 1 {
					c, err = FingerprintMultipart(ctx, u.Label, u.Volumes, unitProgress)
				} else {
					c, err = FingerprintFile(ctx, u.Path, unitProgress)
				}
				if err != nil {
					// If the context is already canceled (external Ctrl-C
					// path), the read error is just the aborted handle
					// surfacing — report the cancellation as the cause.
					if ctxErr := ctx.Err(); ctxErr != nil {
						fail(ctxErr)
					} else {
						fail(err)
					}
					return
				}
				candidates[i] = c // indexed slot: input order survives parallelism
				filesDone.Add(1)
				if progress != nil {
					progress(int(filesDone.Load()), len(units), doneBytes.Load(), total)
				}
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

// statUnit resolves one unit's on-disk size for the progress totals,
// applying the same abs/stat/directory checks the fingerprint itself applies
// so failures surface up front, in input order, with the same messages.
func statUnit(u FingerprintUnit) (int64, error) {
	paths := u.Volumes
	if len(u.Volumes) <= 1 {
		if u.Path == "" {
			return 0, errors.New("history: empty fingerprint unit")
		}
		paths = []string{u.Path}
	}
	var total int64
	for _, p := range paths {
		absolute, err := filepath.Abs(p)
		if err != nil {
			return 0, fmt.Errorf("history: resolve source %s: %w", p, err)
		}
		absolute = filepath.Clean(absolute)
		info, err := os.Stat(absolute)
		if err != nil {
			return 0, fmt.Errorf("history: stat source %s: %w", absolute, err)
		}
		if info.IsDir() {
			return 0, fmt.Errorf("history: source is a directory: %s", absolute)
		}
		total += info.Size()
	}
	return total, nil
}
