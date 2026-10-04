//go:build windows

package selfupdate

import (
	"strings"
	"syscall"
	"unsafe"
)

var procGetLongPathNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLongPathNameW")

// canonicalJournalPath expands every 8.3 short-name component of path
// (C:\Users\RUNNER~1\... → C:\Users\runneradmin\...) via GetLongPathNameW,
// so journal paths written under one spelling compare equal to the same
// directory spelled the other way. Paths that cannot be expanded — e.g.
// components that no longer exist — are returned as-is; the os.SameFile
// fallback in sameJournalDir covers what expansion misses.
func canonicalJournalPath(path string) string {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	// First call queries the required buffer size (no destination buffer).
	n, _, _ := procGetLongPathNameW.Call(uintptr(unsafe.Pointer(p)), 0, 0)
	if n == 0 {
		return path
	}
	buf := make([]uint16, n)
	m, _, _ := procGetLongPathNameW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), n)
	if m == 0 || m > n {
		return path
	}
	return syscall.UTF16ToString(buf)
}

// spellingEqual compares directory spellings the way Windows file systems
// treat paths: case-insensitively.
func spellingEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}
