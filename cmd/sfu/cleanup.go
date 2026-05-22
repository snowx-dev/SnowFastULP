package main

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// paths registry for force-exit cleanup hints.
// graceful path runs deferred handlers, force path (2nd ctrl-c) snapshots
// this and tells user what to rm by hand. never unregistered.

var (
	cleanupMu    sync.Mutex
	cleanupPaths []string
)

// records p as something a force-exit would leave behind. safe from any
// goroutine, dedups so callers can register the same path twice
func registerCleanupPath(p string) {
	if p == "" {
		return
	}
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	for _, existing := range cleanupPaths {
		if existing == p {
			return
		}
	}
	cleanupPaths = append(cleanupPaths, p)
}

func snapshotCleanupPaths() []string {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	out := make([]string, len(cleanupPaths))
	copy(out, cleanupPaths)
	return out
}

// stderr warning for force-exit branch. no-op if nothing survives
func printManualCleanupHint(w io.Writer) {
	paths := snapshotCleanupPaths()
	surviving := paths[:0]
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			surviving = append(surviving, p)
		}
	}
	if len(surviving) == 0 {
		return
	}
	fmt.Fprintln(w, "\nforce-exit: cleanup skipped. Please remove manually:")
	for _, p := range surviving {
		fmt.Fprintln(w, "  "+p)
	}
}
