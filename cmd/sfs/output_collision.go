package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/pathident"
)

// rejects -o clobbering a search target. output opens w/ O_TRUNC pre-scan
// so collisions must be caught early.
// identity via pathident.SameFile (dev/inode unix, handle info windows),
// nonexistent out falls back to clean+abs compare (case-fold on windows)
func ensureNoOutputCollision(outFile string, archives []string) error {
	outFile = strings.TrimSpace(outFile)
	if outFile == "" {
		return nil
	}
	absOut, err := filepath.Abs(outFile)
	if err != nil {
		return fmt.Errorf("resolve -o: %w", err)
	}
	absOut = filepath.Clean(absOut)
	for _, arch := range archives {
		absArch, err := filepath.Abs(arch)
		if err != nil {
			continue
		}
		absArch = filepath.Clean(absArch)
		if same, err := pathident.SameFile(absOut, absArch); err == nil && same {
			return fmt.Errorf("-o would clobber a search target: %s", absArch)
		}
		if pathsLookEqual(absOut, absArch) {
			return fmt.Errorf("-o would clobber a search target: %s", absArch)
		}
	}
	return nil
}
