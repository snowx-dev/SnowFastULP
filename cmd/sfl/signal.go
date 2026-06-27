package main

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// signalContext cancels the run on the first SIGINT/SIGTERM (graceful) and
// force-exits on the second, mirroring sfu/sfs. The returned closure reports
// whether a signal has been seen so callers can label the interrupt.
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
		forceExit("force-exit (signal received twice).")
	}()
	return ctx, cancel, sigFlag.Load
}
