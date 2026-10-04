//go:build !windows

package ulpengine

import (
	"os"
	"syscall"
)

// archiveInstanceDevIno returns the device and inode of the archive file.
// A content-preserving atomic replacement (rename of a new file over the
// pathname) produces a new (dev, ino) pair even when size and mtime are
// restored, so the sidecar's verify cache misses and one digest pass
// re-confirms the new instance. Mirrors internal/index's instance token.
// ok is false when the stat payload is unavailable (degrades to digesting
// every run, never to a false "fresh").
func archiveInstanceDevIno(fi os.FileInfo) (dev, ino uint64, ok bool) {
	st, sok := fi.Sys().(*syscall.Stat_t)
	if !sok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
