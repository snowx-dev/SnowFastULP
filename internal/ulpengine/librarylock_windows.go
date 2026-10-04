//go:build windows

package ulpengine

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockLibraryFile attempts a single non-blocking LockFileEx. Used by
// acquireLibraryLock's retry loop.
func tryLockLibraryFile(f *os.File, mode lockMode) error {
	h := windows.Handle(f.Fd())
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if mode == lockModeShared {
		flags = windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var ol windows.Overlapped
	if err := windows.LockFileEx(h, flags, 0, 1, 0, &ol); err != nil {
		return fmt.Errorf("LockFileEx: %w", err)
	}
	return nil
}

// unlockLibraryFile drops the byte-range lock. The handle close is handled by
// the caller.
func unlockLibraryFile(f *os.File, _ lockMode) error {
	h := windows.Handle(f.Fd())
	var ol windows.Overlapped
	if err := windows.UnlockFileEx(h, 0, 1, 0, &ol); err != nil {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}
