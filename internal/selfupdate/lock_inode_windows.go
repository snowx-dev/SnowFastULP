//go:build windows

package selfupdate

// fileIDFromSys is the Windows stub: os.FileInfo carries no inode here, so
// the lock identity falls back to content/mtime tokens (see lockIdentity).
func fileIDFromSys(sys any) uint64 { return 0 }
