package main

import "sync/atomic"

// shared progress counters for TUI + tests. all fields concurrency-safe.
type metrics struct {
	bytesRead     atomic.Int64 // raw bytes from inputs (P1)
	bytesShard    atomic.Int64 // bytes written to bucket files (P1)
	bytesWritten  atomic.Int64 // bytes to final output (P2)
	linesRead     atomic.Int64 // candidate lines parsed (P1)
	linesAccepted atomic.Int64
	linesRejected atomic.Int64
	linesUnique   atomic.Int64 // unique lines emitted (P2)
	// already-in-library credentials (-od P2). 0 when -od off.
	// sums into "Removed" alongside dups and rejects.
	linesSkippedByDest atomic.Int64

	chunksTotal atomic.Int64
	chunksDone  atomic.Int64

	bucketsTotal atomic.Int64
	bucketsDone  atomic.Int64

	// byte-granular within-bucket progress so the dedup bar doesnt freeze
	// when N workers all start their first bucket simultaneously
	bucketsBytesTotal atomic.Int64
	bucketsBytesRead  atomic.Int64

	activeWorkers atomic.Int32 // in loop
	busyWorkers   atomic.Int32 // inside a chunk/bucket

	totalInputBytes int64 // immutable
	phase           atomic.Int32
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
