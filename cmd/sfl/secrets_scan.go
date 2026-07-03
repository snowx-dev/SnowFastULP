//go:build secrets

package main

import (
	"context"
	"runtime"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// secretSinkAdapter bridges the sflog seam to the scanner pool + store.
type secretSinkAdapter struct {
	pool  *secrets.Pool
	store *secrets.Store
	seen  *secrets.Deduper
}

func (a *secretSinkAdapter) ScanSecrets(ctx context.Context, content []byte, prov string) int {
	// Byte-identical members (rife in stealer dumps) yield identical findings
	// that the store would collapse anyway, so skip the ~tens-of-ms Titus scan
	// for content already scanned this run. The member is still credited as
	// scanned by the caller's defer, so progress still reaches 100%.
	if !a.seen.FirstSight(content) {
		return 0
	}
	fs, err := a.pool.Scan(ctx, content, prov)
	if err != nil {
		return 0 // best-effort: a scan failure never fails an extraction
	}
	for _, f := range fs {
		a.store.Add(f)
	}
	return len(fs)
}

// buildSecretSink constructs the scanner pool + store for -secrets. It returns
// the sink and a close func that tears both down and yields the run's stats.
func buildSecretSink(path string, workers int) (sflog.SecretSink, func() (secrets.Stats, error), error) {
	pool, err := secrets.NewPool(scannerPoolSize(workers))
	if err != nil {
		return nil, nil, err
	}
	store, err := secrets.Open(path)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	dedup := secrets.NewDeduper()
	sink := &secretSinkAdapter{pool: pool, store: store, seen: dedup}
	closeFn := func() (secrets.Stats, error) {
		st, err := store.Close()
		st.Deduped = dedup.Skipped()
		pool.Close()
		return st, err
	}
	return sink, closeFn, nil
}

// scannerPoolSize sizes the Titus scanner pool. Scanning is CPU-bound (~110ms
// per file over Hyperscan's rule DB, independent of file size), so it scales
// with the worker count but never past the core count — extra scanners can't
// run without a core and only cost memory. Each scanner is cheap (~2MB heap,
// built concurrently by NewPool), so a per-core pool is affordable and lets the
// archive-member scan tail finish ~cores× faster than the old flat cap of 4.
func scannerPoolSize(workers int) int {
	n := workers
	if cpu := runtime.NumCPU(); n > cpu {
		n = cpu
	}
	if n < 1 {
		n = 1
	}
	return n
}
