//go:build unix

package fdlimit

import "syscall"

func platformMaxOpenFiles() (int, bool) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, false
	}
	return int(lim.Cur), true
}
