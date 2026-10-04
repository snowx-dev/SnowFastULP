//go:build !unix && !windows

package ulpengine

import (
	"errors"
	"os"
)

// tryLockLibraryFile is unsupported on platforms without a known advisory
// lock primitive; the build matrix targets unix (flock) and windows (LockFileEx).
func tryLockLibraryFile(f *os.File, mode lockMode) error {
	return errLibraryLockUnsupported
}

// unlockLibraryFile is a no-op companion to the stub above.
func unlockLibraryFile(f *os.File, _ lockMode) error { return nil }
