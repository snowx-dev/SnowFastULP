package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

const interruptGrace = 5 * time.Second

// closes tracked archive fds on ctrl-c so blocked Read/ReadAt unsticks.
// workers still running after grace, force-exit 130
func watchInterrupt(ctx context.Context, files *fileabort.Registry, signaled func() bool) {
	if files == nil {
		return
	}
	<-ctx.Done()
	if signaled == nil || !signaled() {
		return
	}
	files.CloseAll()

	timer := time.NewTimer(interruptGrace)
	defer timer.Stop()
	<-timer.C

	restoreTerminal()
	fmt.Fprintln(os.Stderr, "\nforce-exit: interrupted (cleanup timed out)")
	exitWithCode(130)
}
