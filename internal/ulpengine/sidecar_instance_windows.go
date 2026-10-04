//go:build windows

package ulpengine

import "os"

// archiveInstanceDevIno has no cheap stable per-instance analog on Windows
// (no inode exposed via os.FileInfo), so ok is false: the verify cache never
// hits and every run re-digests the archive. Same documented degradation as
// internal/index's Windows instance token — digest-on-every-run is slower
// but never accepts a forged-mtime rewrite as fresh.
func archiveInstanceDevIno(fi os.FileInfo) (dev, ino uint64, ok bool) {
	return 0, 0, false
}
