//go:build !windows

package index

import (
	"fmt"
	"os"
	"syscall"
)

// archiveInstanceToken returns a stable per-instance identity token for the
// file described by fi: device and inode ("dev:ino"). A content-preserving
// atomic replacement (rename of a new file over the pathname) therefore
// produces a new token even when size and mtime are restored, forcing one
// digest re-confirmation for the new instance. Mirrors the unix build-tag
// platform split used by internal/atomicfs.
func archiveInstanceToken(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Unknown stat payload (should not happen on unix builds): fall back
		// to the empty token, which degrades to size+mtime-only matching.
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}
