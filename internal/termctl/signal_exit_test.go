//go:build unix

package termctl

import (
	"context"
	"io"
	"syscall"
	"testing"
	"time"
)

// The interrupt exit code follows the conventional 128+signal split: SIGINT
// stays 130, SIGTERM maps to 143. No recorded signal (defensive default)
// keeps the historical 130.
func TestInterruptExitCodeSignalSplit(t *testing.T) {
	old := receivedSignal.Load()
	t.Cleanup(func() { receivedSignal.Store(old) })

	receivedSignal.Store(0)
	if got := InterruptExitCode(); got != 130 {
		t.Fatalf("no signal: InterruptExitCode() = %d, want 130", got)
	}
	receivedSignal.Store(1)
	if got := InterruptExitCode(); got != 130 {
		t.Fatalf("SIGINT: InterruptExitCode() = %d, want 130", got)
	}
	receivedSignal.Store(2)
	if got := InterruptExitCode(); got != 143 {
		t.Fatalf("SIGTERM: InterruptExitCode() = %d, want 143", got)
	}
}

// SignalContext must record which signal arrived, so a SIGTERM shutdown exits
// 143 through both the graceful path and the force-exit seam. A real SIGTERM
// is delivered to the test process; the force-exit seam is stubbed because a
// second signal would otherwise kill the test binary.
func TestSignalContextRecordsTerminatingSignal(t *testing.T) {
	old := receivedSignal.Load()
	t.Cleanup(func() { receivedSignal.Store(old) })
	SetForceExitHook(func() {})
	t.Cleanup(func() { SetForceExitHook(nil) })

	r := New(io.Discard, nil)
	ctx, cancel, signaled := r.SignalContext()
	t.Cleanup(cancel)

	if code := InterruptExitCode(); code != 130 {
		t.Fatalf("before any signal InterruptExitCode() = %d, want the 130 default", code)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("self-SIGTERM: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !signaled() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !signaled() {
		t.Fatal("SIGTERM did not arm the signaled closure within 5s")
	}
	if err := ctx.Err(); err != context.Canceled {
		t.Fatalf("ctx.Err() = %v, want Canceled after SIGTERM", err)
	}
	if code := InterruptExitCode(); code != 143 {
		t.Fatalf("after SIGTERM InterruptExitCode() = %d, want 143", code)
	}
}
