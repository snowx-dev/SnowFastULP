package termctl

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestoreNilSafe(t *testing.T) {
	reg := New(io.Discard, nil)
	reg.Restore() // nothing set: no-op, must not panic
	reg.Clear()
	reg.Restore()
}

func TestSetClearRestoreSemantics(t *testing.T) {
	reg := New(io.Discard, nil)
	called := 0
	reg.Set(func() { called++ })

	reg.Restore()
	if called != 1 {
		t.Fatalf("Restore after Set: called=%d, want 1", called)
	}
	// Restore does not clear the hook (mirrors the prior registry): a second
	// Restore re-invokes fn. frame.close is idempotent so this is harmless.
	reg.Restore()
	if called != 2 {
		t.Fatalf("second Restore: called=%d, want 2", called)
	}

	reg.Clear()
	reg.Restore()
	if called != 2 {
		t.Fatalf("Restore after Clear: called=%d, want 2 (Clear must make it a no-op)", called)
	}

	// Double Set: the second registration wins.
	first, second := 0, 0
	reg.Set(func() { first++ })
	reg.Set(func() { second++ })
	reg.Restore()
	if first != 0 || second != 1 {
		t.Fatalf("double Set: first=%d second=%d, want first=0 second=1", first, second)
	}
}

func TestForceExitPrepareRunsHintAndReason(t *testing.T) {
	buf := &bytes.Buffer{}
	hintRan := false
	reg := New(buf, func(w io.Writer) {
		hintRan = true
		fmt.Fprintln(w, "HINT-LINE")
	})
	reg.Set(func() {}) // restore hook installed
	reg.forceExitPrepare("REASON-TEXT")

	if !hintRan {
		t.Fatal("cleanupHint was not invoked by forceExitPrepare")
	}
	if !strings.Contains(buf.String(), "HINT-LINE") {
		t.Errorf("hint output missing from out; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "REASON-TEXT") {
		t.Errorf("reason missing from out; got %q", buf.String())
	}
}

func TestForceExitPrepareReasonWithoutHint(t *testing.T) {
	// A nil cleanupHint (sfs case) must still print the reason.
	buf := &bytes.Buffer{}
	reg := New(buf, nil)
	reg.forceExitPrepare("NO-HINT-REASON")
	if !strings.Contains(buf.String(), "NO-HINT-REASON") {
		t.Errorf("reason missing with nil hint; got %q", buf.String())
	}
}

// TestRestoreWithoutHookDoesNotEmitAltScreenLeave runs in a pty so we can
// confirm a Restore with no registered hook emits nothing to the terminal —
// no stray alt-screen leave or scroll-region reset on piped/non-TUI runs.
func TestRestoreWithoutHookDoesNotEmitAltScreenLeave(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty-backed terminal restore test is Unix-only")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for pty-backed terminal restore test")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	script := `
import os, pty, select, sys

exe = os.environ["TERMCTL_TEST_EXE"]
pid, fd = pty.fork()
if pid == 0:
    os.execve(exe, [exe, "-test.run=^TestHelperRestoreNoHook$"], os.environ.copy())

out = bytearray()
while True:
    ready, _, _ = select.select([fd], [], [], 5)
    if not ready:
        break
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    out.extend(chunk)

_, status = os.waitpid(pid, 0)
sys.stdout.buffer.write(out)
code = os.waitstatus_to_exitcode(status)
if code != 0:
    sys.exit(code)
`
	cmd := exec.Command(python, "-c", script)
	cmd.Env = append(os.Environ(),
		"TERMCTL_TEST_EXE="+exe,
		"TERMCTL_RESTORE_HELPER=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pty helper failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte(AltScreenLeave)) {
		t.Fatalf("Restore emitted alt-screen leave without registered hook: %q", out)
	}
	if bytes.Contains(out, []byte(ANSIResetScroll)) {
		t.Fatalf("Restore emitted scroll-region reset without registered hook: %q", out)
	}
}

func TestHelperRestoreNoHook(t *testing.T) {
	if os.Getenv("TERMCTL_RESTORE_HELPER") != "1" {
		return
	}
	reg := New(os.Stderr, nil)
	reg.Clear()
	reg.Restore() // no hook set: must emit nothing
	os.Exit(0)
}

// ForceExit tests need the os.Exit seam stubbed, and every test restores the
// production default: no watcher goroutine is armed in these tests (ForceExit
// runs inline), so a plain defer is enough.
func stubForceExitSeam(t *testing.T) {
	t.Helper()
	SetForceExitHook(func() {})
	t.Cleanup(func() { SetForceExitHook(nil) })
}

func TestExitHookNilSafe(t *testing.T) {
	stubForceExitSeam(t)
	reg := New(io.Discard, nil)
	reg.ForceExit("R") // no exit hook registered: inert, must not panic
}

func TestExitHookFiresBeforeSeamWithReason(t *testing.T) {
	stubForceExitSeam(t)
	buf := &bytes.Buffer{}
	reg := New(buf, nil)
	var order []string
	reg.SetExitHook(func(reason string) { order = append(order, "hook:"+reason) })
	SetForceExitHook(func() { order = append(order, "seam") })
	reg.ForceExit("FORCE-REASON")
	if len(order) != 2 || order[0] != "hook:FORCE-REASON" || order[1] != "seam" {
		t.Fatalf("ForceExit order = %v, want [hook:FORCE-REASON seam] (hook before os.Exit seam)", order)
	}
	if !strings.Contains(buf.String(), "FORCE-REASON") {
		t.Errorf("reason missing from out; got %q", buf.String())
	}
}

func TestClearExitHookInert(t *testing.T) {
	stubForceExitSeam(t)
	reg := New(io.Discard, nil)
	called := 0
	reg.SetExitHook(func(string) { called++ })
	reg.ClearExitHook()
	reg.ClearExitHook() // idempotent
	reg.ForceExit("R")
	if called != 0 {
		t.Fatalf("cleared hook fired %d time(s), want 0", called)
	}
}

func TestSetExitHookConcurrentWithForceExit(t *testing.T) {
	stubForceExitSeam(t)
	reg := New(io.Discard, nil)
	var fired atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 500 {
			reg.SetExitHook(func(string) { fired.Add(1) })
			reg.ClearExitHook()
		}
	}()
	for range 100 {
		reg.ForceExit("r")
	}
	<-done
	_ = fired.Load() // every value is valid: the race detector is the assertion
}

// A hook that blocks forever (e.g. an emitter Stop waiting on a writer stuck
// in a stalled pipe) must not stop ForceExit: the bounded wait expires and
// the os.Exit seam still runs — the hard abort must always reach exit.
func TestExitHookBlockedForeverStillReachesSeam(t *testing.T) {
	stubForceExitSeam(t)
	reg := New(io.Discard, nil)
	entered := make(chan struct{})
	reg.SetExitHook(func(string) {
		close(entered)
		<-make(chan struct{}) // block forever
	})
	var order []string
	ranSeam := make(chan struct{})
	SetForceExitHook(func() {
		order = append(order, "seam")
		close(ranSeam)
	})
	defer SetForceExitHook(func() {})

	start := time.Now()
	go reg.ForceExit("R")
	<-entered
	select {
	case <-ranSeam:
	case <-time.After(10 * time.Second):
		t.Fatal("ForceExit never reached the seam: blocked hook is not bounded")
	}
	elapsed := time.Since(start)
	// Observed runtime is exitHookGrace (2s) itself; 1.5s of scheduling slack
	// keeps this non-flaky under load while still failing fast if the wait
	// ever stops being bounded.
	if elapsed > exitHookGrace+1500*time.Millisecond {
		t.Fatalf("seam ran after %v, want within the %v hook grace + slack", elapsed, exitHookGrace)
	}
	if len(order) != 1 || order[0] != "seam" {
		t.Fatalf("order = %v, want [seam]", order)
	}
}
