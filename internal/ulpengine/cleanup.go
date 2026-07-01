package ulpengine

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
	cleanupLog   []string
)

// records p as something a force-exit would leave behind. safe from any
// goroutine, dedups so callers can register the same path twice
func RegisterCleanupPath(p string) {
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

func SnapshotCleanupPaths() []string {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	out := make([]string, len(cleanupPaths))
	copy(out, cleanupPaths)
	return out
}

// LogCleanupLine records a human-readable cleanup action for the live interrupt
// frame. Safe from any goroutine; lines are append-only for the process lifetime.
func LogCleanupLine(line string) {
	if line == "" {
		return
	}
	cleanupMu.Lock()
	cleanupLog = append(cleanupLog, line)
	cleanupMu.Unlock()
}

// SnapshotCleanupLog returns a copy of cleanup lines logged so far.
func SnapshotCleanupLog() []string {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	out := make([]string, len(cleanupLog))
	copy(out, cleanupLog)
	return out
}

// RemovePathLogged removes a single file and logs the outcome for the interrupt UI.
func RemovePathLogged(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return
		}
		LogCleanupLine(fmt.Sprintf("! could not remove %s: %v", path, err))
		return
	}
	LogCleanupLine(fmt.Sprintf("removed %s", path))
}

// RemoveTreeLogged removes a directory tree and logs the outcome.
func RemoveTreeLogged(path string) {
	if path == "" {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		LogCleanupLine(fmt.Sprintf("! could not remove %s: %v", path, err))
		return
	}
	LogCleanupLine(fmt.Sprintf("removed temp dir %s", path))
}

// FlushRegisteredCleanup attempts to remove every registered path. Missing
// paths are skipped; failures are logged for the interrupt UI.
func FlushRegisteredCleanup() {
	for _, p := range SnapshotCleanupPaths() {
		flushOnePath(p)
	}
}

func flushOnePath(path string) {
	if path == "" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.IsDir() {
		RemoveTreeLogged(path)
		return
	}
	RemovePathLogged(path)
}

// survivingCleanupPaths returns registered paths that still exist on disk.
func survivingCleanupPaths() []string {
	paths := SnapshotCleanupPaths()
	var surviving []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			surviving = append(surviving, p)
		}
	}
	return surviving
}

// PrintManualCleanupHint flushes registered paths first, then warns about any
// that could not be removed. No-op when the registry is empty and everything
// was deleted.
func PrintManualCleanupHint(w io.Writer) {
	FlushRegisteredCleanup()
	surviving := survivingCleanupPaths()
	if len(surviving) == 0 {
		return
	}
	fmt.Fprintln(w, "\ncould not remove automatically — delete manually:")
	for _, p := range surviving {
		fmt.Fprintln(w, "  "+p)
	}
}

// ForceExit restores the terminal, flushes registered scratch paths, prints
// manual hints for survivors, and exits 130. Shared by sfu/sfl force-exit paths.
func ForceExit(restore func(), w io.Writer, reason string) {
	if restore != nil {
		restore()
	}
	PrintManualCleanupHint(w)
	fmt.Fprintln(w, reason)
	os.Exit(130)
}
