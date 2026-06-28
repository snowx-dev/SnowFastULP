package main

import (
	"os"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
)

func indexActivity(metrics *search.Metrics, archiveName string) *zstdframe.Activity {
	if metrics == nil {
		return nil
	}
	return &zstdframe.Activity{
		FrameScan: func(start bool) {
			if start {
				metrics.BeginFrameScan(archiveName)
			} else {
				metrics.EndFrameScan(archiveName)
			}
		},
		Decode: func(start bool) {
			if start {
				metrics.BeginDecode(archiveName)
			} else {
				metrics.EndDecode(archiveName)
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
