//go:build unix

package ulpengine

import (
	"syscall"
	"testing"
)

// lowerFDLimit drops RLIMIT_NOFILE's soft limit to max(28, current soft floor
// of running fds) and registers restore. Skips the test when lowering is not
// permitted (e.g. soft already at or below n).
func lowerFDLimit(t *testing.T, n uint64) {
	t.Helper()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Skipf("getrlimit: %v", err)
	}
	if n >= old.Cur {
		t.Skipf("soft limit already %d, not lowering to %d", old.Cur, n)
	}
	newLim := syscall.Rlimit{Cur: n, Max: old.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &newLim); err != nil {
		t.Skipf("setrlimit: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old)
	})
}
