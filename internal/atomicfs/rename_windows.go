//go:build windows

package atomicfs

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// pin literals, syscall constants vary across Go versions.
// WinError.h: ERROR_ACCESS_DENIED=5, ERROR_SHARING_VIOLATION=32
const (
	winErrAccessDenied     syscall.Errno = 5
	winErrSharingViolation syscall.Errno = 32
)

// retry os.Rename through transient sharing violations.
// AV/Defender/indexers grab brief read shares, 50ms x 5 covers it
func platformRename(src, dst string) error {
	const attempts = 5
	const delay = 50 * time.Millisecond
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := os.Rename(src, dst); err == nil {
			return nil
		} else if !isSharingViolation(err) {
			return err
		} else {
			lastErr = err
		}
		time.Sleep(delay)
	}
	return lastErr
}

func isSharingViolation(err error) bool {
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case winErrAccessDenied, winErrSharingViolation:
			return true
		}
	}
	return false
}
