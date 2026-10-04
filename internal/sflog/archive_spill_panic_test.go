package sflog

import (
	"context"
	"os"
	"testing"
)

// panicAfterReader serves limit bytes, then panics, simulating a hostile
// nested decoder blowing up mid-spill: the panic crosses spillToTemp before
// the caller's recoverAsError sees it, so cleanup must not depend on the copy
// returning normally.
type panicAfterReader struct {
	served int
}

func (r *panicAfterReader) Read(p []byte) (int, error) {
	if r.served >= 3 {
		panic("decoder boom mid-spill")
	}
	n := len(p)
	if n > 3-r.served {
		n = 3 - r.served
	}
	for i := range n {
		p[i] = 'x'
	}
	r.served += n
	return n, nil
}

// A reader panic mid-copy must not leak the open temp file, the temp file
// itself, or the spill budget: close/remove/release are armed with the
// allocation and disarmed only when the spill succeeds.
func TestSpillToTempCleansUpOnReaderPanic(t *testing.T) {
	dir := t.TempDir()
	budget := newSpillBudget(0, 0)

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_, _, _ = spillToTemp(context.Background(), dir, "inner.zip", &panicAfterReader{}, nil, budget)
	}()
	if panicked == nil {
		t.Fatal("spillToTemp swallowed the reader panic")
	}
	if got := budget.used.Load(); got != 0 {
		t.Fatalf("spill budget kept %d bytes after a panic; want 0", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("spill directory kept temp files after a panic: %v", names)
	}
}
