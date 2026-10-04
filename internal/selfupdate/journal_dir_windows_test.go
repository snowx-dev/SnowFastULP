//go:build windows

package selfupdate

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

var procGetShortPathNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetShortPathNameW")

// shortPathName returns the 8.3 short spelling of long via GetShortPathNameW,
// or the input unchanged when the volume generates no short names.
func shortPathName(long string) string {
	p, err := syscall.UTF16PtrFromString(long)
	if err != nil {
		return long
	}
	n, _, _ := procGetShortPathNameW.Call(uintptr(unsafe.Pointer(p)), 0, 0)
	if n == 0 {
		return long
	}
	buf := make([]uint16, n)
	m, _, _ := procGetShortPathNameW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), n)
	if m == 0 || m > n {
		return long
	}
	return syscall.UTF16ToString(buf)
}

// TestSameJournalDirWindowsShortNames pins the real 8.3 expansion: a journal
// directory reached by its 8.3 short-name spelling must validate against the
// long spelling and vice versa, exactly as when t.TempDir() hands the
// journal a RUNNER~1 path while recovery derives runneradmin. Runs in the
// test-selfupdate-windows CI job; skipped where 8.3 names are disabled.
func TestSameJournalDirWindowsShortNames(t *testing.T) {
	long := t.TempDir()
	short := shortPathName(long)
	if strings.EqualFold(short, long) {
		t.Skip("volume does not generate 8.3 short names for this path")
	}
	if !strings.Contains(short, "~1") {
		t.Fatalf("GetShortPathNameW returned no short form: %q", short)
	}
	if !sameJournalDir(long, short) {
		t.Fatalf("short-name spelling of the journal directory rejected: dir=%q entryDir=%q", long, short)
	}
	if !sameJournalDir(short, long) {
		t.Fatalf("long-name spelling of the journal directory rejected: dir=%q entryDir=%q", short, long)
	}
	if canonicalJournalPath(short) == short {
		t.Fatalf("canonicalJournalPath left the short name unexpanded: %q", short)
	}
	if !spellingEqual(`C:\A\B`, `c:\a\b`) {
		t.Fatal("Windows spelling comparison must be case-insensitive")
	}
	if sameJournalDir(long, filepath.Join(t.TempDir(), "other")) {
		t.Fatal("different directory accepted as the journal directory")
	}
}
