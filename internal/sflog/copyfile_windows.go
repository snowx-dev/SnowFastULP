//go:build windows

package sflog

import (
	"os"

	"golang.org/x/sys/windows"
)

// openReadNoFollow opens path for reading without following a final reparse
// point (symlink, junction, mount point), closing the Lstat→Open TOCTOU that
// copyFile otherwise has.
func openReadNoFollow(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, windows.ERROR_CANT_RESOLVE_FILENAME
	}
	return os.NewFile(uintptr(h), path), nil
}
