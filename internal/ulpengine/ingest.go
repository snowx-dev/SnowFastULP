package ulpengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// IngestOptions drives an in-process -od ingest of a single ULP file into an
// existing library directory. It reproduces the Config sfu builds for
// `sfu -od <LibraryDir> <ULPPath>` so the resulting library is byte-identical
// to the standalone tool.
type IngestOptions struct {
	ULPPath    string // source ULP file to merge into the library
	LibraryDir string // -od destination (created if missing)

	Workers       int       // phase-1 parser goroutines, 0 = auto
	DedupWorkers  int       // phase-2 dedup goroutines, 0 = auto
	Buckets       int       // 0 = adaptive
	TempDir       string    // shard temp parent, "" = library dir
	NoURI         bool      // emit host:login:password (drop URL path/query)
	FastPathOff   bool      // disable the in-RAM fast path
	ZstChunkLines int64     // split granularity, 0 = DefaultZstChunkLines
	RunStarted    time.Time // run clock origin, zero = time.Now()
	RunStamp      string    // "<YYYYMMDD>_<id>", "" = derived
	DryRun        bool      // -odr: full pipeline but no library writes

	// OnResolved, if set, is invoked once the run is resolved (after the
	// dest-dedup metrics are attached, before Run starts) so a caller can grab
	// r.OdMetrics for a live phase-0 progress view. Must not retain r beyond the
	// call or mutate it.
	OnResolved func(r *Resolved)

	// Debug, if set, receives the engine's structured ingest events (shard/dedup
	// phases, -od scan classify/regen/done). The caller owns its lifecycle
	// (Close it after Ingest returns).
	Debug *DebugLog
}

// Ingest merges opts.ULPPath into the library at opts.LibraryDir in-process,
// identical to `sfu -od <LibraryDir> <ULPPath>`. Live progress is published to
// m (poll its atomic counters from a TUI). It returns the resolved run so
// callers can report the phase-0 outcome (OdResult) and final library size.
//
// The new archive is named sfu_<stamp>.txt.zst so the library's discovery and
// dedup keep treating it as a first-class member regardless of which tool wrote
// it.
func Ingest(ctx context.Context, opts IngestOptions, m *Metrics) (*Resolved, error) {
	if opts.ULPPath == "" {
		return nil, fmt.Errorf("ingest: ULP path is required")
	}
	if opts.LibraryDir == "" {
		return nil, fmt.Errorf("ingest: library dir is required")
	}

	started := opts.RunStarted
	if started.IsZero() {
		started = time.Now()
	}
	stamp := opts.RunStamp
	if stamp == "" {
		id, err := NewRunID()
		if err != nil {
			return nil, fmt.Errorf("ingest: run id: %w", err)
		}
		stamp = RunStamp(started, id)
	}
	chunkLines := opts.ZstChunkLines
	if chunkLines == 0 {
		chunkLines = DefaultZstChunkLines
	}

	outDirAbs, err := filepath.Abs(opts.LibraryDir)
	if err != nil {
		return nil, fmt.Errorf("ingest: resolve library dir: %w", err)
	}
	// dry-run never creates the library dir; od_scan treats a missing dest as
	// an empty library and the would-be output lands in the per-run temp dir.
	if !opts.DryRun {
		if err := os.MkdirAll(outDirAbs, 0o755); err != nil {
			return nil, fmt.Errorf("ingest: create library dir: %w", err)
		}
	}
	absOut, err := filepath.Abs(filepath.Join(outDirAbs, WithZstExt(DefaultBasename(stamp), true)))
	if err != nil {
		return nil, fmt.Errorf("ingest: resolve output: %w", err)
	}

	// Mirror the exact Config sfu's main builds for -od (compress forced on;
	// sidecar regen + dedup only read .zst archives).
	cfg := Config{
		Inputs:        []string{opts.ULPPath},
		Output:        absOut,
		TempDir:       opts.TempDir,
		Workers:       opts.Workers,
		DedupWorkers:  opts.DedupWorkers,
		Buckets:       opts.Buckets,
		FastPathOff:   opts.FastPathOff,
		Compress:      true,
		ZstChunkLines: chunkLines,
		RunStarted:    started,
		RunStamp:      stamp,
		NoURI:         opts.NoURI,
		DestDedup:     true,
		DestDedupDir:  outDirAbs,
		DryRun:        opts.DryRun,
		Debug:         opts.Debug,
	}

	r, err := Resolve(cfg)
	if err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}
	EnsureDestDedupMetrics(r)
	if opts.OnResolved != nil {
		opts.OnResolved(r)
	}

	// best-effort sweep of orphan shard dirs from crashed runs (mirrors sfu)
	if err := os.MkdirAll(r.TempDir, 0o755); err == nil {
		SweepStaleWorkDirs(r.TempDir, "")
	}

	if err := Run(ctx, r, m); err != nil {
		return r, err
	}
	return r, nil
}
