//go:build windows

package selfupdate

import (
	"errors"

	"golang.org/x/sys/windows"
)

// pidProvablyDead reports whether the process pid is demonstrably no longer
// running on this host, via OpenProcess plus a zero-timeout wait on its
// exit handle. H-13: the update lock may only be stolen when its holder is
// provably dead AND past the staleness window; a live holder (slow serial
// payload downloads can exceed lockStaleAge) keeps its lock however old it
// is.
//
// It returns false whenever the probe cannot decide: an unusable pid, an
// unexpected OpenProcess error, or a wait failure. An undecided holder is
// treated as live, so an ambiguous case delays the steal (the held-lock
// error advises the operator to re-run or delete the lock) rather than
// risking a concurrent apply. ERROR_ACCESS_DENIED means the process exists
// but our privileges cannot wait on it — that is live, not dead.
func pidProvablyDead(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if h != 0 {
		defer windows.CloseHandle(h)
		event, werr := windows.WaitForSingleObject(h, 0)
		return werr == nil && event == uint32(windows.WAIT_OBJECT_0)
	}
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return true // no such process
	}
	// ERROR_ACCESS_DENIED (process exists, privileges insufficient) and
	// anything else: undecided, treated as live.
	return false
}
