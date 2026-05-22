package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// ctx cancelled on SIGINT/SIGTERM, returns sigFlag closure too.
// SIGTERM is unix-only, windows covers ctrl-c via os.Interrupt.
// first signal = graceful, second = force-exit
func signalContext() (context.Context, context.CancelFunc, func() bool) {
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
		<-ch
		restoreTerminal()
		fmt.Fprintln(os.Stderr, "force-exit (signal received twice).")
		exitWithCode(130)
	}()
	return ctx, cancel, sigFlag.Load
}
