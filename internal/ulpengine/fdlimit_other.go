//go:build !unix

package ulpengine

import "github.com/snowx-dev/SnowFastULP/internal/fdlimit"

func maxOpenFiles() (int, bool) { return fdlimit.MaxOpenFiles() }
