package main

import (
	"fmt"
	"os"
)

// parsed CLI positionals
type searchArgs struct {
	Root    string
	Pattern string
}

// PATTERN alone -> cwd, DIR PATTERN -> DIR
func parseSearchArgs(positionals []string) (searchArgs, error) {
	switch len(positionals) {
	case 1:
		if isExistingDir(positionals[0]) {
			return searchArgs{}, fmt.Errorf("directory %q given but no pattern; usage: sfs DIR PATTERN", positionals[0])
		}
		return searchArgs{Root: ".", Pattern: positionals[0]}, nil
	case 2:
		// catches `sfs ./logins.zst foo` typo, would silently scan parent dir
		if !isExistingDir(positionals[0]) {
			return searchArgs{}, fmt.Errorf("first arg must be a directory; got %q (usage: sfs DIR PATTERN)", positionals[0])
		}
		return searchArgs{Root: positionals[0], Pattern: positionals[1]}, nil
	default:
		return searchArgs{}, fmt.Errorf("need PATTERN or DIR PATTERN")
	}
}

func isExistingDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
