package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

const interruptGrace = 5 * time.Second

// watchInterrupt unsticks the run after a graceful Ctrl-C. Closing the tracked
// file handles unblocks any Read/ReadAt stuck in kernel I/O on slow storage so
// workers notice the cancelled context promptly. If they are still draining
// after a grace window, force-exit 130 rather than hang.
func watchInterrupt(ctx context.Context, files *fileabort.Registry, signaled func() bool) {
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

	fmt.Fprintln(os.Stderr, "\nforce-exit: interrupted (cleanup timed out)")
	exitWithCode(130)
}
