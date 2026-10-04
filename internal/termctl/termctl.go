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

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"

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

// InterruptGrace is the window workers get to drain after a graceful Ctrl-C
// before a force-exit unsticks them. Exported so the interrupt frames can
// state the real deadline instead of an aspirational promise.
const InterruptGrace = 5 * time.Second

// RestoreRegistry holds the live TUI's teardown hook so any exit path (a
// second Ctrl-C, a fatal error, a cleanup timeout) can leave the alt-screen
// and bring the cursor back through one mutex-guarded path. The hook is
// installed by the monitor/runUI goroutine via Set and cleared on its way
// out via Clear; Restore is a no-op until Set runs and after Clear runs.
type RestoreRegistry struct {
	mu          sync.Mutex
	fn          func()
	exitFn      func(reason string)
	flushFn     func()
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

// SetExitHook installs fn as the pre-exit hook ForceExit invokes after
// forceExitPrepare (terminal restored, hint + reason printed) and before the
// os.Exit seam runs. The -json streams use it to close the NDJSON stream
// with an interrupted terminal + summary before a force-exit kills the
// process. Registered once from the run goroutine early and invoked later
// from the signal/watcher goroutine — the same mutex discipline as the
// restore hook: grabbed under the mutex, invoked outside the lock so a
// concurrent Set/Clear can't tear the pointer mid-call. Nil default = inert
// (sfs never registers one).
func (r *RestoreRegistry) SetExitHook(fn func(reason string)) {
	r.mu.Lock()
	r.exitFn = fn
	r.mu.Unlock()
}

// ClearExitHook removes the pre-exit hook. Idempotent.
func (r *RestoreRegistry) ClearExitHook() {
	r.SetExitHook(nil)
}

// SetExitFlush installs fn as a best-effort flush of buffered run artifacts
// (the -debug logs and -debug-reject recorder) that runs on every process
// exit seam: ExitWithCode calls it inline, ForceExit runs it inside the same
// bounded grace as the exit hook. Every fatal/usage/interrupt path funnels
// through ExitWithCode and every hard abort through ForceExit, and both end
// in os.Exit, which never runs deferred closes — without this hook anything
// still sitting in a debug log's 64KB bufio buffer is lost on exactly the
// abnormal exits the log exists to diagnose. Registered once from the run
// goroutine right where the artifacts are created and invoked later from the
// signal/exit path — the same mutex discipline as the restore and exit
// hooks: grabbed under the mutex, invoked outside the lock. The flush must
// be silent and best-effort: a dying process must not mask its real fatal
// message with flush noise, and a flush failure must never alter the exit
// code. Nil default = inert (no buffered artifacts).
func (r *RestoreRegistry) SetExitFlush(fn func()) {
	r.mu.Lock()
	r.flushFn = fn
	r.mu.Unlock()
}

// ClearExitFlush removes the flush hook. Idempotent. Called when the buffered
// artifact closes and later exits have nothing left to flush.
func (r *RestoreRegistry) ClearExitFlush() {
	r.SetExitFlush(nil)
}

// runExitFlush invokes the installed flush hook outside the mutex. Silent by
// contract; a flush failure is swallowed on purpose.
func (r *RestoreRegistry) runExitFlush() {
	r.mu.Lock()
	fn := r.flushFn
	r.mu.Unlock()
	if fn != nil {
		fn()
	}
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
// exit paths; prints no hint and no reason. The exit flush runs first: it is
// silent, and the buffered debug tail should be on disk even if the restore
// path were to stall.
func (r *RestoreRegistry) ExitWithCode(code int) {
	r.runExitFlush()
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
// the terminal, print the manual-cleanup hint (if any), print reason, run the
// registered exit flush (debug artifacts), run the pre-exit hook (jsonout
// stream close; nil by default), then exit 130. It does NOT call
// ExitWithCode (that would double-Restore). reason is printed even when
// cleanupHint is nil.
//
// The hook is a best-effort grace, never a new way to hang: it runs on its
// own goroutine with a bounded wait, so a hook blocked forever (e.g. the
// NDJSON emitter's Stop waiting on a writer stuck in a stalled pipe, or a
// flush wedged on pathological storage) cannot
// stop ForceExit from reaching os.Exit — the hard abort must always exit.
// An abandoned hook goroutine dies with the process; the emitter's
// exactly-once guard means a late-completing hook can never emit a second
// terminal.
//
// If the hook races a graceful shutdown already inside the emitter's Stop
// (first-writer-wins once-guard), the stream's terminal is the graceful one
// (done, or an interrupted with an empty error field) while the process still
// exits 130 with the force-exit reason on stderr — intentional: the exit
// code and the reason on stderr are authoritative, and the last two lines
// stay a valid terminal → summary pair either way. In the abandoned case
// (the hook itself stuck), the stream instead ends on a possibly truncated
// update line: os.Exit kills the mid-write emitter goroutine. The exit code
// and stderr reason remain authoritative there too.
const exitHookGrace = 2 * time.Second

func (r *RestoreRegistry) ForceExit(reason string) {
	r.forceExitPrepare(reason)
	r.mu.Lock()
	fn := r.exitFn
	fl := r.flushFn
	r.mu.Unlock()
	if fl != nil || fn != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if fl != nil {
				fl()
			}
			if fn != nil {
				fn(reason)
			}
		}()
		timer := time.NewTimer(exitHookGrace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}
	runForceExitHook()
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
		case sig := <-ch:
			if sig == syscall.SIGTERM {
				receivedSignal.Store(2)
			} else {
				receivedSignal.Store(1)
			}
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
// after InterruptGrace, ForceExit(reasonCleanupTimeout) rather than hang.
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

	timer := time.NewTimer(InterruptGrace)
	defer timer.Stop()
	<-timer.C

	r.ForceExit(reasonCleanupTimeout)
}

// receivedSignal records which signal started the interrupt (1 = SIGINT,
// 2 = SIGTERM, 0 = none yet). SignalContext sets it on the first signal;
// InterruptExitCode reads it. Atomic because the signal/watcher goroutine
// writes while exit paths read.
var receivedSignal atomic.Int32

// InterruptExitCode reports the conventional exit code for the signal that
// interrupted the run: 143 for SIGTERM, 130 for SIGINT (128+signal). Runs
// interrupted without a recorded signal (defensively) stay at 130, the
// historical value.
func InterruptExitCode() int {
	if receivedSignal.Load() == 2 {
		return exitcode.Terminated
	}
	return exitcode.Interrupted
}

// forceExitHook is the os.Exit seam under ForceExit: production exits with
// the interrupt exit code (130 for SIGINT, 143 for SIGTERM), tests stub it
// (like the other run() exit seams) so an armed watcher left
// over from a stubbed graceful exit cannot fire a delayed process kill into
// a later test. Held behind atomic.Pointer because ForceExit runs on a
// signal/watcher goroutine while tests swap the hook — a plain package var
// would be a data race.
var forceExitHook atomic.Pointer[func()]

func init() {
	SetForceExitHook(nil) // install the production default
}

// SetForceExitHook replaces the force-exit seam; nil restores the production
// default (os.Exit with the interrupt exit code). Safe to call concurrently
// with the watcher goroutine that will eventually run it.
func SetForceExitHook(fn func()) {
	if fn == nil {
		def := func() { os.Exit(InterruptExitCode()) }
		forceExitHook.Store(&def)
		return
	}
	forceExitHook.Store(&fn)
}

// runForceExitHook invokes the installed seam.
func runForceExitHook() {
	if fn := forceExitHook.Load(); fn != nil {
		(*fn)()
	}
}
