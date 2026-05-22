//go:build windows

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// lazy load, no cost when preflight is skipped
var (
	modKernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = modKernel32.NewProc("GetDiskFreeSpaceExW")
)

// per-user free bytes via GetDiskFreeSpaceExW (honours quota)
func diskFree(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("path encode %s: %w", path, err)
	}
	var freeAvail, totalBytes, totalFree uint64
	r1, _, e1 := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		if e1 != nil && e1 != syscall.Errno(0) {
			return 0, fmt.Errorf("GetDiskFreeSpaceExW %s: %w", path, e1)
		}
		return 0, fmt.Errorf("GetDiskFreeSpaceExW %s: failed", path)
	}
	return freeAvail, nil
}

// drive letter or UNC share match. junctions across volumes are rare,
// false negative just causes both volumes to be checked separately
func sameVolume(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}
