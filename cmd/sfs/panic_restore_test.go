package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
)

// A render panic inside the runUI goroutine must leave the alt screen and show
// the cursor — reset/show-cursor/leave-alt after the frame entered — before the
// panic re-surfaces out of the goroutine. The seam fires right after a frame
// render, i.e. after alt-screen registration.
func TestSfsUIPanicRestore(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	path := f.Name()
	defer os.Remove(path)

	oldStderr := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = oldStderr; f.Close() })

	oldTTY := stderrIsTTY
	stderrIsTTY = func() bool { return true }
	t.Cleanup(func() { stderrIsTTY = oldTTY })

	oldHook := drawPanicHook
	drawPanicHook = func() { panic("render boom") }
	t.Cleanup(func() { drawPanicHook = oldHook })

	done := make(chan struct{})
	cfg := uiConfig{
		Mode:    uiFull,
		Metrics: &search.Metrics{},
		Pattern: "boom",
		Start:   time.Now(),
		Done:    done,
	}
	surfaced := make(chan any, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				surfaced <- r
			}
		}()
		runUI(cfg, nil)
	}()

	// The 100ms ticker fires the first render, then the seam panics with the
	// alt screen up. Done stays open until the panic surfaces so the first
	// tick cannot be preempted by a clean exit.
	select {
	case r := <-surfaced:
		if r != "render boom" {
			t.Fatalf("surfaced panic = %v, want the render panic re-raised", r)
		}
	case <-time.After(5 * time.Second):
		close(done)
		t.Fatal("runUI never panicked")
	}

	f.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	enter := strings.Index(out, termctl.AltScreenEnter)
	leave := strings.LastIndex(out, termctl.AltScreenLeave)
	if enter < 0 || leave < 0 || leave < enter {
		t.Fatalf("alt screen never restored after panic:\n%q", out)
	}
	hide := strings.Index(out, termctl.ANSIHideCursor)
	show := strings.LastIndex(out, termctl.ANSIShowCursor)
	if hide < 0 || show < 0 || show < hide {
		t.Fatalf("cursor never shown after panic:\n%q", out)
	}
	if rest := out[leave+len(termctl.AltScreenLeave):]; strings.Contains(rest, termctl.AltScreenEnter) {
		t.Fatalf("frame re-entered the alt screen after panic restore:\n%q", rest)
	}
}

// main's outer guard restores the terminal on any panic in the run goroutine
// while the TUI goroutine owns the alt screen, then re-panics so the crash
// surfaces.
func TestSfsMainPanicRestore(t *testing.T) {
	restored := false
	reg.Set(func() { restored = true })
	defer reg.Clear()
	surfaced := func() (r any) {
		defer func() { r = recover() }()
		defer restoreOnPanic()
		panic("main boom")
	}()
	if !restored {
		t.Fatal("terminal restore hook not run before the panic surfaced")
	}
	if surfaced != "main boom" {
		t.Fatalf("surfaced panic = %v, want the original panic re-raised", surfaced)
	}
}
