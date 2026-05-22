//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// bytes available to unprivileged user, matches df -h
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// same filesystem? compares st_dev. missing paths return false
func sameVolume(a, b string) bool {
	sa, ea := os.Stat(a)
	sb, eb := os.Stat(b)
	if ea != nil || eb != nil {
		return false
	}
	da, oka := sa.Sys().(*syscall.Stat_t)
	db, okb := sb.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return false
	}
	return da.Dev == db.Dev
}
