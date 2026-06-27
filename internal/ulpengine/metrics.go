package ulpengine

import "sync/atomic"

// shared progress counters for TUI + tests. all fields concurrency-safe.
type Metrics struct {
	BytesRead     atomic.Int64 // raw bytes from inputs (P1)
	BytesShard    atomic.Int64 // bytes written to bucket files (P1)
	BytesWritten  atomic.Int64 // bytes to final output (P2)
	LinesRead     atomic.Int64 // candidate lines parsed (P1)
	LinesAccepted atomic.Int64
	LinesRejected atomic.Int64
	LinesUnique   atomic.Int64 // unique lines emitted (P2)
	// already-in-library credentials (-od P2). 0 when -od off.
	// sums into "Removed" alongside dups and rejects.
	LinesSkippedByDest atomic.Int64

	ChunksTotal atomic.Int64
	ChunksDone  atomic.Int64

	BucketsTotal atomic.Int64
	BucketsDone  atomic.Int64

	// byte-granular within-bucket progress so the dedup bar doesnt freeze
	// when N workers all start their first bucket simultaneously
	BucketsBytesTotal atomic.Int64
	BucketsBytesRead  atomic.Int64

	ActiveWorkers atomic.Int32 // in loop
	BusyWorkers   atomic.Int32 // inside a chunk/bucket

	TotalInputBytes int64 // immutable
	Phase           atomic.Int32
}

const (
	phaseInit int32 = iota
	// -od only: discovery + sidecar regen + dest-key routing. without this
	// the TUI sat at "PARSING 0%" for tens of minutes on long regens
	phasePhase0
	phaseShard
	phaseDedup
	// -od only: post-dedup pass writes .idx sidecars for this runs own
	// archives. used to run invisibly between dedup 100% and recap
	phaseIndex
	phaseDone
)
