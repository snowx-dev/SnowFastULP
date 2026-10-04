package sflog

import (
	"bytes"
	"context"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeEnvSeed writes a distinct env-candidate file under root.
func writeEnvSeed(t *testing.T, root string, i int) string {
	t.Helper()
	p := filepath.Join(root, fmt.Sprintf("secrets-%03d.env", i))
	body := fmt.Sprintf("TOKEN=%d\nSECRET=unique-%d\n", i, i)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The copy queue holds 256 slots; without a running worker the 257th enqueue
// blocks forever. Enqueue must lazily start the worker (void call sites — cmd
// never calls Start), so enqueueing far past the queue size cannot deadlock and
// Close drains every accepted job.
func TestEnvCopierEnqueuePast256WithoutStartDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	copier := NewEnvCopier(filepath.Join(t.TempDir(), "env-dest"), nil, EnvCopyMaxLen)

	const total = 300
	done := make(chan struct{}, total)
	for i := range total {
		go func(i int) {
			copier.EnqueueFile(writeEnvSeed(t, dir, i))
			done <- struct{}{}
		}(i)
	}
	for n := 0; n < total; n++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("EnqueueFile deadlocked at job %d of %d without an explicit Start", n, total)
		}
	}
	es := copier.Close()
	if es.Copied != total {
		t.Fatalf("Copied = %d, want %d (every accepted job drained)", es.Copied, total)
	}
}

// After Close, a late enqueue must be rejected without panicking and recorded
// as a run error (CallbackError), never silently accepted into a closed queue.
func TestEnvCopierLateEnqueueAfterCloseRejected(t *testing.T) {
	dir := t.TempDir()
	copier := NewEnvCopier(filepath.Join(t.TempDir(), "env-dest"), nil, EnvCopyMaxLen)

	p := writeEnvSeed(t, dir, 0)
	copier.EnqueueFile(p)
	if es := copier.Close(); es.Copied != 1 {
		t.Fatalf("Copied = %d, want 1", es.Copied)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("late enqueue panicked: %v", r)
		}
	}()
	copier.EnqueueFile(p)
	if copier.CallbackError() == nil {
		t.Fatal("late enqueue after Close must record a run error (CallbackError), not succeed silently")
	}
}

// Engine.Run must cancel the copier so a full copy queue can never block
// worker shutdown: after Run returns, a late enqueue is rejected (non-blocking)
// and recorded as a run error.
func TestEngineCancellationCancelsEnvCopier(t *testing.T) {
	root := t.TempDir()
	writeEnvSeed(t, root, 0)

	copier := NewEnvCopier(filepath.Join(t.TempDir(), "env-dest"), nil, EnvCopyMaxLen)
	var out bytes.Buffer
	eng := &Engine{Workers: 1, Passwords: []string{""}, EnvCopier: copier}
	if _, _, err := eng.Run(context.Background(), root, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	done := make(chan struct{})
	go func() {
		copier.EnqueueFile(writeEnvSeed(t, root, 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-Run enqueue blocked: Engine did not cancel the copier")
	}
	if copier.CallbackError() == nil {
		t.Fatal("enqueue after engine cancellation must record a run error (CallbackError)")
	}
}

// Enqueue racing Close must never panic (send on closed channel) and Close
// must stay idempotent. Run under -race by the suite.
func TestEnvCopierEnqueueCloseRace(t *testing.T) {
	dir := t.TempDir()
	copier := NewEnvCopier(filepath.Join(t.TempDir(), "env-dest"), nil, EnvCopyMaxLen)
	paths := make([]string, 8)
	for i := range paths {
		paths[i] = writeEnvSeed(t, dir, i)
	}

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			copier.EnqueueFile(paths[i%len(paths)])
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		copier.Close()
	}()
	wg.Wait()
	// Idempotent second close: no panic, no double-drain.
	copier.Close()
}

// blockingHasher stalls the copy worker inside its first job until release is
// closed, so the queue fills up behind it.
type blockingHasher struct{ release chan struct{} }

func (b *blockingHasher) Write(p []byte) (int, error) { return len(p), nil }
func (b *blockingHasher) Sum(_ []byte) []byte         { b0 := make([]byte, 8); return b0 }
func (b *blockingHasher) Reset()                      {}
func (b *blockingHasher) Size() int                   { return 8 }
func (b *blockingHasher) BlockSize() int              { return 64 }
func (b *blockingHasher) Sum64() uint64               { <-b.release; return 0 }

// Cancelling a copier whose single worker is stalled and whose 256-slot queue
// is full must unblock pending enqueues immediately (recorded as a run error,
// never a copy issue), and Close must still drain every accepted job.
func TestEnvCopierCancelUnblocksFullQueue(t *testing.T) {
	dir := t.TempDir()
	copier := NewEnvCopier(filepath.Join(t.TempDir(), "env-dest"), nil, EnvCopyMaxLen)

	release := make(chan struct{})
	oldHasher := newEnvHasher
	newEnvHasher = func() hash.Hash64 { return &blockingHasher{release: release} }
	t.Cleanup(func() { newEnvHasher = oldHasher })

	const total = 300 // 1 in-flight + 256 queued; the rest block on enqueue
	done := make(chan struct{}, total)
	for i := range total {
		go func(i int) {
			copier.EnqueueFile(writeEnvSeed(t, dir, i))
			done <- struct{}{}
		}(i)
	}
	for n := 0; n < 257; n++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of the expected 257 accepted enqueues completed", n)
		}
	}

	// Cancellation must unblock the remaining enqueues without any copy issue.
	copier.cancelRun()
	for n := 0; n < total-257; n++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("cancellation did not unblock blocked enqueue (remaining %d)", total-257-n)
		}
	}
	if copier.CallbackError() == nil {
		t.Fatal("cancelled enqueue must record a run error (CallbackError)")
	}

	// Unblock the stalled worker; Close drains every accepted job.
	close(release)
	es := copier.Close()
	if es.Copied != 257 {
		t.Fatalf("Copied = %d, want 257 (accepted jobs drained, rejected ones not counted)", es.Copied)
	}
	if len(es.Issues) != 0 {
		t.Fatalf("issues = %+v, want none: cancellation is a run error, not a copy issue", es.Issues)
	}
}
