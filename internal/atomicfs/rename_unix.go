//go:build !windows

package atomicfs

import "os"

func platformRename(src, dst string) error { return os.Rename(src, dst) }
