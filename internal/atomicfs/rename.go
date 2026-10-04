// Package atomicfs holds atomic-style filesystem helpers.
package atomicfs

// Rename moves src to dst, retries on Windows sharing violations.
func Rename(src, dst string) error { return platformRename(src, dst) }
