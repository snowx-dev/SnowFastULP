package secrets

import (
	"sync"
	"testing"
)

func TestDeduperFirstSight(t *testing.T) {
	d := NewDeduper()
	a := []byte("aws_secret=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	b := []byte("different content entirely")

	if !d.FirstSight(a) {
		t.Fatal("first sight of a must report new")
	}
	if d.FirstSight(a) {
		t.Fatal("second sight of identical a must report duplicate")
	}
	if !d.FirstSight(b) {
		t.Fatal("first sight of distinct b must report new")
	}
	if got := d.Skipped(); got != 1 {
		t.Fatalf("Skipped = %d, want 1 (only a repeated)", got)
	}
}

// TestDeduperNilIsNoOp guards the disabled path: a nil Deduper always reports
// "new" so callers never skip a scan when dedup was not wired.
func TestDeduperNilIsNoOp(t *testing.T) {
	var d *Deduper
	if !d.FirstSight([]byte("x")) || !d.FirstSight([]byte("x")) {
		t.Fatal("nil Deduper must always report new")
	}
	if d.Skipped() != 0 {
		t.Fatal("nil Deduper must report 0 skipped")
	}
}

// TestDeduperConcurrentSameContent proves that under N concurrent FirstSight
// calls for identical content, exactly one wins (reports new) and the rest are
// counted as skipped — the invariant that keeps a shared buffer scanned once.
func TestDeduperConcurrentSameContent(t *testing.T) {
	d := NewDeduper()
	const n = 64
	content := []byte("ghp_1234567890abcdefghijklmnopqrstuvwx12")
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		new int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.FirstSight(content) {
				mu.Lock()
				new++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if new != 1 {
		t.Fatalf("exactly one caller must see new content, got %d", new)
	}
	if got := d.Skipped(); got != n-1 {
		t.Fatalf("Skipped = %d, want %d", got, n-1)
	}
}
