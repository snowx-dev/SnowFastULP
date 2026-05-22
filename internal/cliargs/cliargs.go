// Package cliargs holds argv-parsing helpers shared by sfu and sfs.
package cliargs

import (
	"flag"
	"strings"
)

// IsVersionRequest reports whether argv asks for the version banner.
func IsVersionRequest(argv []string) bool {
	for _, a := range argv {
		switch a {
		case "--version", "-version", "version":
			return true
		}
	}
	return false
}

// IsHelpRequest reports whether argv asks for help.
func IsHelpRequest(argv []string) bool {
	for _, a := range argv {
		switch a {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

// SplitPositional separates flag tokens from positionals, preserving flag VALUE pairs.
// fs is consulted to know if a flag consumes the next token. nil = flag.CommandLine.
// bare `-` is positional (stdin sentinel).
func SplitPositional(argv []string, fs *flag.FlagSet) ([]string, []string) {
	if fs == nil {
		fs = flag.CommandLine
	}
	flags := []string{}
	pos := []string{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			pos = append(pos, argv[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if strings.Contains(a, "=") {
				continue
			}
			name := strings.TrimLeft(a, "-")
			fl := fs.Lookup(name)
			if fl == nil {
				continue
			}
			if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				continue
			}
			if i+1 < len(argv) {
				flags = append(flags, argv[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

// SplitTrailingParen splits a desc into (main, " (suffix)") when it ends w/ balanced parens.
// suffix INCLUDES leading space and parens. used by help renderers to dim default tails.
func SplitTrailingParen(s string) (main, suffix string) {
	if !strings.HasSuffix(s, ")") {
		return s, ""
	}
	if i := strings.LastIndex(s, " ("); i > 0 {
		return s[:i], s[i:]
	}
	return s, ""
}
