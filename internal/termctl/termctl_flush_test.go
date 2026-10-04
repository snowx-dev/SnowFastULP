package termctl

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// record seq under a mutex: ForceExit runs the flush on its own goroutine
// while the test goroutine calls ForceExit.
type seqLog struct {
	mu  sync.Mutex
	seq []string
}

func (s *seqLog) add(ev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq = append(s.seq, ev)
}

func (s *seqLog) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seq...)
}

// ForceExit must run the registered exit flush before the force-exit seam:
// the debug tail reaches disk before os.Exit kills the process.
func TestForceExitRunsExitFlushBeforeSeam(t *testing.T) {
	log := &seqLog{}
	reg := New(&bytes.Buffer{}, nil)
	reg.Set(func() {}) // restore hook installed, like a live TUI
	reg.SetExitFlush(func() { log.add("flush") })
	SetForceExitHook(func() { log.add("seam") })
	t.Cleanup(func() { SetForceExitHook(nil) })

	reg.ForceExit("REASON")
	got := log.events()
	if len(got) != 2 || got[0] != "flush" || got[1] != "seam" {
		t.Fatalf("ForceExit sequence = %v, want [flush seam]", got)
	}
}

// ForceExit with only a flush and no -json hook (the sfs shape) must
// still run the flush inside the bounded grace and reach the exit seam.
func TestForceExitRunsLoneExitFlush(t *testing.T) {
	log := &seqLog{}
	reg := New(&bytes.Buffer{}, nil)
	reg.SetExitFlush(func() { log.add("flush") })
	SetForceExitHook(func() { log.add("seam") })
	t.Cleanup(func() { SetForceExitHook(nil) })

	reg.ForceExit("REASON")
	got := log.events()
	if len(got) != 2 || got[0] != "flush" || got[1] != "seam" {
		t.Fatalf("ForceExit sequence = %v, want [flush seam]", got)
	}
}

// ClearExitFlush detaches the hook: a later force-exit must not touch the
// (already closed) artifact, and the seam must still be reached.
func TestForceExitSkipsClearedExitFlush(t *testing.T) {
	log := &seqLog{}
	reg := New(&bytes.Buffer{}, nil)
	reg.SetExitFlush(func() { log.add("flush") })
	reg.ClearExitFlush()
	SetForceExitHook(func() { log.add("seam") })
	t.Cleanup(func() { SetForceExitHook(nil) })

	reg.ForceExit("REASON")
	if got := log.events(); len(got) != 1 || got[0] != "seam" {
		t.Fatalf("ForceExit sequence = %v, want [seam] only", got)
	}
}

// No flush and no exit hook at all (the historical default): ForceExit must
// reach the seam unchanged, and a nil flush registration is inert.
func TestForceExitReachesSeamWithoutHooks(t *testing.T) {
	log := &seqLog{}
	reg := New(&bytes.Buffer{}, nil)
	reg.SetExitFlush(nil)
	SetForceExitHook(func() { log.add("seam") })
	t.Cleanup(func() { SetForceExitHook(nil) })

	reg.ForceExit("REASON")
	if got := log.events(); len(got) != 1 || got[0] != "seam" {
		t.Fatalf("ForceExit sequence = %v, want [seam] only", got)
	}
}

// ExitWithCode ends in os.Exit and cannot be called in-process, so the funnel
// runs in a helper subprocess (the same pattern as the pty restore test): the
// helper registers a flush that drains a simulated buffered artifact, then
// exits 7. The parent asserts the tail reached the file and the exit code is
// byte-identical to the pre-flush behavior — a flush must never change it.
// Fails pre-fix: the simulated buffer is never drained and the file is empty.
func TestExitWithCodeRunsExitFlush(t *testing.T) {
	if os.Getenv("TERMCTL_EXITFLUSH_HELPER") == "1" {
		reg := New(os.Stderr, nil)
		f, err := os.Create(os.Getenv("TERMCTL_EXITFLUSH_FILE"))
		if err != nil {
			os.Exit(3)
		}
		w := bufio.NewWriterSize(f, 4096)
		fmt.Fprint(w, "buffered-tail")
		reg.SetExitFlush(func() { _ = w.Flush() })
		reg.ExitWithCode(7)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "artifact.log")
	cmd := exec.Command(exe, "-test.run=^TestExitWithCodeRunsExitFlush$")
	cmd.Env = append(os.Environ(),
		"TERMCTL_EXITFLUSH_HELPER=1",
		"TERMCTL_EXITFLUSH_FILE="+file,
	)
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("helper: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7 (flush must not alter the code)", code)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "buffered-tail" {
		t.Fatalf("ExitWithCode did not run the flush; file = %q", raw)
	}
}
