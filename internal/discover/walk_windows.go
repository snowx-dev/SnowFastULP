//go:build windows

package discover

import (
	"io/fs"
	"strings"
	"syscall"
)

// skip dot-prefixed + hidden/system dirs (AppData, $Recycle.Bin, etc).
// matches default Explorer view skipping
func shouldSkipDir(path string, d fs.DirEntry) bool {
	if strings.HasPrefix(d.Name(), ".") {
		return true
	}
	info, err := d.Info()
	if err != nil {
		return false
	}
	wd, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || wd == nil {
		return false
	}
	if wd.FileAttributes&(syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_SYSTEM) != 0 {
		return true
	}
	return false
}
