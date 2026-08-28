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

// parseSearchArgs resolves positionals. In fMode (-f), the PATTERN is supplied
// via the terms file, so only an optional DIR root is accepted: 0 positionals
// (root left empty for applyFModeRoot) or 1 positional that must be an existing
// directory. A bare PATTERN positional is rejected with a clear message.
func parseSearchArgs(positionals []string) (searchArgs, error) {
	return parseSearchArgsMode(positionals, false)
}

func parseSearchArgsMode(positionals []string, fMode bool) (searchArgs, error) {
	switch len(positionals) {
	case 0:
		if !fMode {
			return searchArgs{}, fmt.Errorf("need PATTERN or DIR PATTERN")
		}
		// Empty root: applyFModeRoot fills [sfs].dir or CWD.
		return searchArgs{Root: "", Pattern: ""}, nil
	case 1:
		if fMode {
			if !isExistingDir(positionals[0]) {
				return searchArgs{}, fmt.Errorf("with -f, the optional first arg must be a directory; got %q (usage: sfs -f FILE [DIR])", positionals[0])
			}
			return searchArgs{Root: positionals[0], Pattern: ""}, nil
		}
		if isExistingDir(positionals[0]) {
			return searchArgs{}, fmt.Errorf("directory %q given but no pattern; usage: sfs DIR PATTERN", positionals[0])
		}
		return searchArgs{Root: ".", Pattern: positionals[0]}, nil
	case 2:
		if fMode {
			return searchArgs{}, fmt.Errorf("cannot pass a PATTERN with -f; the terms come from the -f FILE (usage: sfs -f FILE [DIR])")
		}
		// catches `sfs ./logins.zst foo` typo, would silently scan parent dir
		if !isExistingDir(positionals[0]) {
			return searchArgs{}, fmt.Errorf("first arg must be a directory; got %q (usage: sfs DIR PATTERN)", positionals[0])
		}
		return searchArgs{Root: positionals[0], Pattern: positionals[1]}, nil
	default:
		if fMode {
			return searchArgs{}, fmt.Errorf("too many arguments with -f (usage: sfs -f FILE [DIR])")
		}
		return searchArgs{}, fmt.Errorf("need PATTERN or DIR PATTERN")
	}
}

func isExistingDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// applyFModeRoot picks the search root when -f is used with no DIR
// positional: the parsed root if set, else [sfs].dir, else CWD.
func applyFModeRoot(parsedRoot, sfsDir string) string {
	if parsedRoot != "" {
		return parsedRoot
	}
	if sfsDir != "" {
		return sfsDir
	}
	return "."
}
