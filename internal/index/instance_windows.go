//go:build windows

package index

import "os"

// archiveInstanceToken returns the platform instance token for the file
// described by fi. Windows has no cheap stable inode analog accessible via
// os.FileInfo's syscall payload, so the token is empty and freshness falls
// back to size+mtime matching only.
//
// Platform difference (documented tradeoff): on Unix an archive rewritten to
// different content with a forged same mtime is still re-hashed once when the
// file was replaced via rename (new dev:ino) or shares the instance only when
// rewritten in place. On Windows, a same-size rewrite with a forged mtime is
// accepted as fresh until the mtime actually changes — the same guarantee
// level as the pre-cache digest-on-every-run check for that exotic case is
// NOT preserved there; first sight of each new (size, mtime) pair is still
// digest-checked.
func archiveInstanceToken(fi os.FileInfo) string {
	return ""
}
