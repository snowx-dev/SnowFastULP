// Package pathdisp formats filesystem paths for human-facing CLI summaries.
package pathdisp

import (
	"os"
	"path/filepath"
	"strings"
)

// ForDisplay returns a path suitable for summary footers: relative to the
// process CWD when the path is under CWD, otherwise an absolute form.
// Parenthesized notes like "(no matches)" are returned unchanged.
func ForDisplay(path string) string {
	if path == "" || strings.HasPrefix(path, "(") {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	cwd, err := os.Getwd()
	if err != nil {
		return abs
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") {
		return abs
	}
	return rel
}
