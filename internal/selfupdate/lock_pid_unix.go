//go:build !windows

package selfupdate

import (
	"errors"
	"os"
	"syscall"
)

// pidProvablyDead reports whether the process pid is demonstrably no longer
// running on this host, via the signal-0 liveness probe. H-13: the update
// lock may only be stolen when its holder is provably dead AND past the
// staleness window; a live holder (slow serial payload downloads can exceed
// lockStaleAge) keeps its lock however old it is.
//
// It returns false whenever the probe cannot decide: an unusable pid, a
// FindProcess failure, or an unexpected probe error. An undecided holder is
// treated as live, so an ambiguous case delays the steal (the held-lock
// error advises the operator to re-run or delete the lock) rather than
// risking a concurrent apply. EPERM from the probe means the process exists
// but belongs to another user — that is live, not dead.
func pidProvablyDead(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err != nil && errors.Is(err, os.ErrProcessDone)
}
