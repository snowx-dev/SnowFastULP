package index

import (
	"sync/atomic"
	"testing"
)

func TestArchiveByteProgressMonotonic(t *testing.T) {
	var done atomic.Int64
	p := NewArchiveByteProgress(&done)
	cb := p.Callback()

	cb(0, 100)
	cb(40, 100)
	cb(40, 100) // dup
	cb(100, 100)
	if got := done.Load(); got != 100 {
		t.Fatalf("done = %d, want 100", got)
	}

	p.Finish(100)
	if got := done.Load(); got != 100 {
		t.Fatalf("after finish done = %d, want 100", got)
	}
}

func TestArchiveByteProgressFinishCreditsLoadPath(t *testing.T) {
	var done atomic.Int64
	p := NewArchiveByteProgress(&done)
	p.Finish(1024)
	if got := done.Load(); got != 1024 {
		t.Fatalf("done = %d, want 1024", got)
	}
}

func TestArchiveByteProgressIgnoresRegression(t *testing.T) {
	var done atomic.Int64
	p := NewArchiveByteProgress(&done)
	cb := p.Callback()
	cb(50, 100)
	cb(10, 100)
	if got := done.Load(); got != 50 {
		t.Fatalf("done = %d, want 50", got)
	}
}
