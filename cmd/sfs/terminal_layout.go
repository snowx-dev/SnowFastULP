package main

import (
	"os"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
)

func indexActivity(metrics *search.Metrics) *zstdframe.Activity {
	if metrics == nil {
		return nil
	}
	return &zstdframe.Activity{
		FrameScan: func(start bool) {
			if start {
				metrics.BeginFrameScan()
			} else {
				metrics.EndFrameScan()
			}
		},
		Decode: func(start bool) {
			if start {
				metrics.BeginDecode()
			} else {
				metrics.EndDecode()
			}
		},
	}
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
