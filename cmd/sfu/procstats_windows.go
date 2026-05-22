//go:build windows

package main

import (
	"runtime"
	"syscall"
	"time"
)

// user+kernel CPU time. Filetime is 100-ns intervals, widen to int64 ns first
// so the conversion is monotonic and cant overflow on long runs
func processCPUTime() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0
	}
	const hundredNs = 100
	ktNs := (uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)) * hundredNs
	utNs := (uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)) * hundredNs
	return time.Duration(ktNs + utNs)
}

// approx RSS via runtime.MemStats.Sys. avoids importing x/sys/windows
// just to call K32GetProcessMemoryInfo for a TUI cosmetic. Sys tracks
// RSS close enough for our alloc pattern (large long-lived hashmaps)
func currentRSSBytes() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Sys
}
