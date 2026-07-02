package main

import (
	"context"
	"path/filepath"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

const secretsDBName = "sfl-secrets.sqlite"

// maxSecretScanners caps the Titus scanner pool. Each scanner compiles ~500
// rules (≈1s and tens of MB), so we don't spin up one per worker on big boxes;
// secret files are small and few, so a handful of concurrent scanners is plenty.
const maxSecretScanners = 4

// resolveSecretsPath picks where the secrets DB lives: explicit flag wins, else
// the output dir, else the library dir, else the current directory.
func resolveSecretsPath(flag, outDir, libDir string) string {
	switch {
	case flag != "":
		return flag
	case outDir != "":
		return filepath.Join(outDir, secretsDBName)
	case libDir != "":
		return filepath.Join(libDir, secretsDBName)
	default:
		return secretsDBName
	}
}

// secretSinkAdapter bridges the sflog seam to the scanner pool + store.
type secretSinkAdapter struct {
	pool  *secrets.Pool
	store *secrets.Store
}

func (a *secretSinkAdapter) ScanSecrets(ctx context.Context, content []byte, prov string) {
	fs, err := a.pool.Scan(ctx, content, prov)
	if err != nil {
		return // best-effort: a scan failure never fails an extraction
	}
	for _, f := range fs {
		a.store.Add(f)
	}
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
	sink := &secretSinkAdapter{pool: pool, store: store}
	closeFn := func() (secrets.Stats, error) {
		st, err := store.Close()
		pool.Close()
		return st, err
	}
	return sink, closeFn, nil
}

func scannerPoolSize(workers int) int {
	switch {
	case workers < 1:
		return 1
	case workers > maxSecretScanners:
		return maxSecretScanners
	default:
		return workers
	}
}
