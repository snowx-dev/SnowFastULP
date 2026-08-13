//go:build unix

package sflog

import (
	"os"

	"golang.org/x/sys/unix"
)

// openReadNoFollow opens path for reading without following a final symlink,
// closing the Lstat→Open TOCTOU that copyFile otherwise has.
func openReadNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
