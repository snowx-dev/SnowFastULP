package ulpengine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// leading dot keeps dir hidden from ls
const tempSubdirPrefix = ".sfu-tmp-"

const staleTempDirAge = 24 * time.Hour

// creates .sfu-tmp-<stamp>-<pid> under parent
func PrepareTempDir(parent, stamp string) (subdir string, err error) {
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create temp parent: %w", err)
	}
	if stamp == "" {
		stamp = "anon"
	}
	pid := os.Getpid()
	name := fmt.Sprintf("%s%s-%d", tempSubdirPrefix, stamp, pid)
	sub := filepath.Join(parent, name)
	if err := os.Mkdir(sub, 0o700); err != nil {
		return "", fmt.Errorf("create temp subdir: %w", err)
	}
	return sub, nil
}

// removes old .sfu-tmp-* dirs except exclude, skips fresh ones to avoid
// stomping on concurrent runs
func SweepStaleTempDirs(parent, exclude string) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0
	}
	removed := 0
	cutoff := time.Now().Add(-staleTempDirAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, tempSubdirPrefix) {
			continue
		}
		if exclude != "" && name == exclude {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(parent, name)); err == nil {
			removed++
		}
	}
	return removed
}
