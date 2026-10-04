//go:build unix

package ulpengine

import (
	"os"
	"syscall"
)

// tryLockLibraryFile attempts a single non-blocking flock. Used by
// acquireLibraryLock's retry loop.
func tryLockLibraryFile(f *os.File, mode lockMode) error {
	how := syscall.LOCK_EX
	if mode == lockModeShared {
		how = syscall.LOCK_SH
	}
	return syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
}

// unlockLibraryFile drops the flock. The fd close is handled by the caller.
func unlockLibraryFile(f *os.File, _ lockMode) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
