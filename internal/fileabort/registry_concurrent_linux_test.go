//go:build linux

package fileabort_test

import (
	"os"
	"testing"
)

// coarse FD count via /proc/self/fd, linux CI only
func countOpenFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}
