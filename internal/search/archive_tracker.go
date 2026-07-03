package search

import "sync"

// archiveTracker tallies how many units (chunks for .zst archives, 1 per plain
// file) remain per archive ord, firing onDone exactly once per ord when its
// last unit finishes, and bumping the shared ChunksDone/BytesChunkDone
// counters per unit. It collapses the markChunkDone/markFileDone +
// bumpChunk/bumpFile pairs that were duplicated between search.go (chunked,
// OnArchiveDone) and search_txt.go (one-per-file, OnFileDone).
type archiveTracker struct {
	remaining map[int]int64
	mu        sync.Mutex
	metrics   *Metrics
	onDone    func(ord int)
}

// newArchiveTracker returns an empty tracker. onDone is OnArchiveDone or
// OnFileDone (nil = silent completion). metrics may be nil.
func newArchiveTracker(metrics *Metrics, onDone func(ord int)) *archiveTracker {
	return &archiveTracker{remaining: make(map[int]int64), metrics: metrics, onDone: onDone}
}

// seed records that ord has n units to finish before it's done. Call once per
// ord before workers start; n is int64(len(sc.Chunks)) for chunked archives,
// 1 for plain files.
func (t *archiveTracker) seed(ord int, n int64) {
	t.mu.Lock()
	t.remaining[ord] = n
	t.mu.Unlock()
}

// markDone decrements ord's remaining units; on the last one it bumps
// ArchivesDone and fires onDone. Pre-cancelled tasks (n already 0/never seeded)
// must not call markDone, matching the prior guard-free contract.
func (t *archiveTracker) markDone(ord int) {
	t.mu.Lock()
	t.remaining[ord]--
	done := t.remaining[ord] == 0
	if done && t.metrics != nil {
		t.metrics.ArchivesDone.Add(1)
	}
	t.mu.Unlock()
	if done && t.onDone != nil {
		t.onDone(ord)
	}
}

// bump advances the per-unit (chunk/file) counters: one ChunksDone, plus
// BytesChunkDone by unitBytes when non-zero.
func (t *archiveTracker) bump(unitBytes int64) {
	if t.metrics == nil {
		return
	}
	t.metrics.ChunksDone.Add(1)
	if unitBytes > 0 {
		t.metrics.BytesChunkDone.Add(unitBytes)
	}
}
