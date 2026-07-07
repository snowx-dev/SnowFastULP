package secrets

import (
	"crypto/sha256"
	"sync"
	"sync/atomic"
)

// Deduper skips re-scanning byte-identical member content within a run. Stealer
// dumps repeat content heavily (duplicate cookie/autofill exports, the same
// archive copied across victim folders), and a Titus scan costs ~tens of ms per
// KB of candidate-rich text — orders of magnitude more than a SHA-256 of the
// same bytes — so scanning each distinct content once is a large net win.
//
// It is safe because identical bytes yield identical findings, and the store
// already dedups by (rule_id, secret) keeping the first source_path: skipping a
// repeat never changes which secrets or provenance are stored. It only affects
// seen_count, which then counts distinct-content sightings rather than every
// byte-for-byte copy — arguably the more meaningful signal.
//
// Concurrency-safe: the parallel member scanners share one Deduper.
type Deduper struct {
	mu      sync.Mutex
	seen    map[[32]byte]struct{}
	skipped atomic.Int64
}

// NewDeduper returns an empty Deduper ready for concurrent use.
func NewDeduper() *Deduper {
	return &Deduper{seen: make(map[[32]byte]struct{})}
}

// FirstSight records content's digest and reports whether it had NOT been seen
// before this call. A false return means an identical buffer was already scanned
// this run, so the caller may skip the (far costlier) secret scan. A nil Deduper
// always reports true, so dedup is a no-op when one was never wired.
func (d *Deduper) FirstSight(content []byte) bool {
	if d == nil {
		return true
	}
	sum := sha256.Sum256(content)
	d.mu.Lock()
	_, seen := d.seen[sum]
	if !seen {
		d.seen[sum] = struct{}{}
	}
	d.mu.Unlock()
	if seen {
		d.skipped.Add(1)
	}
	return !seen
}

// Skipped is the number of scans avoided because their content had already been
// scanned this run.
func (d *Deduper) Skipped() int64 {
	if d == nil {
		return 0
	}
	return d.skipped.Load()
}
