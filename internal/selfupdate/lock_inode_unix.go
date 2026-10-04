//go:build !windows

package selfupdate

import "syscall"

// fileIDFromSys extracts the inode from an os.FileInfo's raw syscall data
// on unix platforms; 0 means the token is unavailable (see lockIdentity).
func fileIDFromSys(sys any) uint64 {
	if st, ok := sys.(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
