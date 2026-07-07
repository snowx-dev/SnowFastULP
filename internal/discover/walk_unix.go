//go:build !windows

package discover

import (
	"io/fs"
	"strings"
)

// skip dot-prefixed dirs (XDG/VCS metadata)
func shouldSkipDir(d fs.DirEntry) bool {
	return strings.HasPrefix(d.Name(), ".")
}
