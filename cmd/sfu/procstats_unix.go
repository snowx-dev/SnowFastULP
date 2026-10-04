//go:build unix

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// user+system CPU time via getrusage. supported uniformly on linux/darwin/BSD
func processCPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Sec)*time.Second +
		time.Duration(ru.Utime.Usec)*time.Microsecond +
		time.Duration(ru.Stime.Sec)*time.Second +
		time.Duration(ru.Stime.Usec)*time.Microsecond
}

// linux: /proc/self/statm (live RSS).
// darwin/BSD: getrusage ru_maxrss (peak, not live, but monotonic and
// better than "always 0"). avoids pulling in x/sys/darwin for task_info()
func currentRSSBytes() uint64 {
	if runtime.GOOS == "linux" {
		if v, ok := readLinuxStatmRSS(); ok {
			return v
		}
	}
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if ru.Maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "linux" {
		return uint64(ru.Maxrss) * 1024 // KiB on linux
	}
	return uint64(ru.Maxrss) // bytes on darwin/BSD
}

func readLinuxStatmRSS() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, false
	}
	var rssPages uint64
	if _, err := fmt.Sscanf(fields[1], "%d", &rssPages); err != nil {
		return 0, false
	}
	return rssPages * uint64(syscall.Getpagesize()), true
}
