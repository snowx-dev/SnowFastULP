//go:build windows

package history

import "golang.org/x/sys/windows"

func renameNoReplace(oldPath, newPath string) error {
	oldPointer, err := windows.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPointer, err := windows.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	// MoveFile, unlike MoveFileEx with MOVEFILE_REPLACE_EXISTING, fails when
	// newPath already exists and therefore provides the required no-replace move.
	return windows.MoveFile(oldPointer, newPointer)
}
