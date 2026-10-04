//go:build !unix

package ulpengine

import "testing"

// lowerFDLimit has no portable implementation off unix; the FD-limit test
// skips there (Windows lock-file CI covers the matrix, not rlimits).
func lowerFDLimit(t *testing.T, _ uint64) {
	t.Skip("fd limit lowering unsupported on this platform")
}

func errTooManyFDs(err error) bool { return false }
