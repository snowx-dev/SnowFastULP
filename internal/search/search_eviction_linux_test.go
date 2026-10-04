//go:build linux

package search_test

import (
	"os"
	"path/filepath"
	"strings"
)

// count /proc/self/fd entries pointing inside dir,
// catches fd accumulation across worker pool transitions
func procArchiveFDCount(dir string) int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(target, dir) {
			n++
		}
	}
	return n
}
