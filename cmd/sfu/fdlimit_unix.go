//go:build unix

package main

import "github.com/snowx-dev/SnowFastULP/internal/fdlimit"

func maxOpenFiles() (int, bool) { return fdlimit.MaxOpenFiles() }
