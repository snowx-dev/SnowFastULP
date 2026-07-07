// Package termctl owns the alt-screen lifecycle (enter/leave, cursor hide/show,
// scroll-region reset) and the single restore-and-exit registry shared by the
// sfu/sfs/sfl CLIs. One registry per process replaces the three near-identical
// terminalRestore/exitWithCode/forceExit/signalContext/watchInterrupt copies
// that previously lived in each cmd.
//
// out must be an unbuffered writer (os.Stderr); ExitWithCode does not flush.
package termctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

// Alt-screen lifecycle escapes. Restore emits ANSIResetScroll + ANSIShowCursor +
// AltScreenLeave; open emits AltScreenEnter + ANSIHideCursor.
const (
	ANSIResetScroll = "\033[r"
	ANSIHideCursor  = "\033[?25l"
	ANSIShowCursor  = "\033[?25h"
	AltScreenEnter  = "\033[?1049h"
	AltScreenLeave  = "\033[?1049l"
)

// Force-exit reason strings, kept here so all three binaries share one source
// of truth for the user-visible messages.
const (
	reasonSecondSignal   = "force-exit (signal received twice)."
	reasonCleanupTimeout = "\nforce-exit: interrupted (cleanup timed out)"
)

// interruptGrace is the window workers get to drain after a graceful Ctrl-C
// before a second force-exit unsticks them.
const interruptGrace = 5 * time.Second

// RestoreRegistry holds the live TUI's teardown hook so any exit path (a
// second Ctrl-C, a fatal error, a cleanup timeout) can leave the alt-screen
// and bring the cursor back through one mutex-guarded path. The hook is
// installed by the monitor/runUI goroutine via Set and cleared on its way
// out via Clear; Restore is a no-op until Set runs and after Clear runs.
type RestoreRegistry struct {
	mu          sync.Mutex
	fn          func()
	out         io.Writer
	cleanupHint func(io.Writer)
}

// New returns a registry bound to out (used for force-exit reason + hint
// output) and an optional cleanupHint printed on force-exit. Pass nil for
// CLIs without a manual-cleanup hint (sfs); pass ulpengine.PrintManualCleanupHint
// for sfu/sfl.
func New(out io.Writer, cleanupHint func(io.Writer)) *RestoreRegistry {
	return &RestoreRegistry{out: out, cleanupHint: cleanupHint}
}

// Set installs fn as the active restore hook. Called by the monitor/runUI
// goroutine once the alt-screen is up.
func (r *RestoreRegistry) Set(fn func()) {
	r.mu.Lock()
	r.fn = fn
	r.mu.Unlock()
}

// Clear removes the restore hook. Idempotent.
func (r *RestoreRegistry) Clear() {
	r.Set(nil)
}

// Restore runs the active restore hook; no-op when nothing is registered.
// The hook is grabbed under the mutex and invoked outside the lock so a
// concurrent Set/Clear can't tear the pointer mid-call.
func (r *RestoreRegistry) Restore() {
	r.mu.Lock()
	fn := r.fn
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ExitWithCode restores the terminal then exits with code. Used by graceful
// exit paths; prints no hint and no reason.
func (r *RestoreRegistry) ExitWithCode(code int) {
	r.Restore()
	os.Exit(code)
}

// forceExitPrepare does everything ForceExit does except os.Exit, so tests can
// assert the hint ran and the reason was written without dying.
func (r *RestoreRegistry) forceExitPrepare(reason string) {
	r.Restore()
	if r.cleanupHint != nil {
		r.cleanupHint(r.out)
	}
	fmt.Fprintln(r.out, reason)
}

// ForceExit handles a hard abort (second Ctrl-C or cleanup timeout): restore
// the terminal, print the manual-cleanup hint (if any), print reason, then
// exit 130. It does NOT call ExitWithCode (that would double-Restore). reason
// is printed even when cleanupHint is nil.
func (r *RestoreRegistry) ForceExit(reason string) {
	r.forceExitPrepare(reason)
	os.Exit(130)
}

// SignalContext returns a context cancelled on the first SIGINT/SIGTERM
// (graceful) plus a signaled closure reporting whether a signal caused the
// cancel. A second signal calls ForceExit(reasonSecondSignal). SIGTERM is
// Unix-only; on Windows os.Interrupt covers Ctrl-C.
func (r *RestoreRegistry) SignalContext() (context.Context, context.CancelFunc, func() bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var sigFlag atomic.Bool
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(ch)
		select {
		case <-ch:
			sigFlag.Store(true)
			cancel()
		case <-ctx.Done():
			return
		}
		// 2nd-signal wait is unconditional: selecting on (ch | ctx.Done)
		// races because ctx is already cancelled and Go would silently
		// swallow every other 2nd Ctrl-C.
		<-ch
		r.ForceExit(reasonSecondSignal)
	}()
	return ctx, cancel, sigFlag.Load
}

// WatchInterrupt unsticks the run after a graceful Ctrl-C. Closing the tracked
// file handles unblocks any Read/ReadAt stuck in kernel I/O on slow storage so
// workers notice the cancelled context promptly. If they are still draining
// after interruptGrace, ForceExit(reasonCleanupTimeout) rather than hang.
// No-op when files is nil or the cancel was natural completion (not a signal).
func (r *RestoreRegistry) WatchInterrupt(ctx context.Context, files *fileabort.Registry, signaled func() bool) {
	if files == nil {
		return
	}
	<-ctx.Done()
	if signaled == nil || !signaled() {
		return // cancelled by normal completion, not a signal
	}
	files.CloseAll()

	timer := time.NewTimer(interruptGrace)
	defer timer.Stop()
	<-timer.C

	r.ForceExit(reasonCleanupTimeout)
}
