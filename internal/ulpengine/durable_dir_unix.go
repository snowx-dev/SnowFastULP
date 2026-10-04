//go:build !windows

package ulpengine

import "os"

// durableSyncDirOS opens the directory read-only and fsyncs it, flushing the
// renamed directory entries to disk. Unix-only; Windows uses a documented
// no-op (durable_dir_windows.go) because portable directory FlushFileBuffers
// is not supported there.
func durableSyncDirOS(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
