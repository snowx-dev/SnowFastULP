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

// BareValuer is a flag.Value that admits a bare form: the flag token with no
// value at all. SplitPositional passes BareFlagValue() to Set for the bare
// form instead of letting the flag package reject the missing value. This is
// how -json means stdout with no argument.
type BareValuer interface {
	flag.Value
	BareFlagValue() string
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
// bare `-` is positional (stdin sentinel). A flag whose Value implements
// BareValuer (OutTarget) also accepts the bare no-value form; its space form
// (-flag VALUE) consumes VALUE like any non-bool flag.
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
			if strings.Contains(a, "=") {
				flags = append(flags, a)
				continue
			}
			name := strings.TrimLeft(a, "-")
			fl := fs.Lookup(name)
			if fl == nil {
				flags = append(flags, a)
				continue
			}
			if bf, ok := fl.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				flags = append(flags, a)
				continue
			}
			// A BareValuer (OutTarget) also accepts the bare form: no value
			// token at all. When the next token is flag-shaped (or absent),
			// the flag is bare — swallowing a flag-shaped token as the value
			// would silently turn "-json -no-tui" into a file named
			// "-no-tui". A non-flag next token is the space form's value:
			// "-json stats.jsonl". The bare form synthesizes the =value
			// spelling so the flag package sees a well-formed token.
			if bv, bare := fl.Value.(BareValuer); bare && (i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "-")) {
				flags = append(flags, a+"="+bv.BareFlagValue())
				continue
			}
			flags = append(flags, a)
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
