//go:build !linux

package fileabort_test

import "testing"

// no portable impl off linux, -1 = skip
func countOpenFDs(t *testing.T) int {
	t.Helper()
	return -1
}
