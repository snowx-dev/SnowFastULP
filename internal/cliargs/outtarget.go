package cliargs

import (
	"fmt"
	"strings"
)

// OutTarget is a flexible flag.Value for stream targets like -json:
//
//	-json            enable, write to stdout (bare form: no value token)
//	-json=-          enable, write to stdout
//	-json=FILE       enable, write to FILE (created/truncated)
//	-json FILE       same as =FILE: SplitPositional consumes FILE as the
//	                     value (only when FILE does not start with '-')
//	-json=false      disable
//	-json=file:NAME  literal file NAME, even when NAME is reserved
//	                     (file:stdout, file:true, file:false, file:-)
//
// The bare form works through cliargs.BareValuer: SplitPositional passes
// BareFlagValue() ("true") to Set when the flag appears with no value token,
// so the flag package never sees a valueless flag. A flag-shaped token after
// the flag is never swallowed as a value: "-json -no-tui" means stdout
// plus -no-tui, not a file named "-no-tui" — a literal filename starting
// with '-' needs the = form. The word "stdout" is accepted so config users
// can spell `json_out = "stdout"` like "-".
type OutTarget struct {
	// Enabled is true once the flag was seen and not disabled.
	Enabled bool
	// Path is the output file, or "" for stdout.
	Path string
	// Literal records the explicit file: escape so reserved spellings such as
	// file:- remain distinguishable after flag parsing.
	Literal bool
}

// String implements flag.Value.
func (t *OutTarget) String() string {
	if t == nil || !t.Enabled {
		return ""
	}
	if t.Path == "" {
		return "-"
	}
	return t.Path
}

// Set implements flag.Value. "true" (the bare flag), "", "-", and "stdout"
// all mean stdout; "false" disables; anything else is a file path.
//
// The "file:" prefix is the explicit literal-path escape: file:stdout,
// file:true, file:false, and file:- create those literal filenames, and
// exactly one prefix is stripped (file:file:x is the file "file:x"). It is
// needed because "true", "false", "-", and "stdout" are control spellings —
// only file: forces the literal name.
func (t *OutTarget) Set(v string) error {
	if p, ok := strings.CutPrefix(v, "file:"); ok {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("json: empty file: escape; use -json (stdout) or file:FILE")
		}
		t.Enabled, t.Path, t.Literal = true, p, true
		return nil
	}
	t.Literal = false
	switch v {
	case "", "true", "-", "stdout":
		t.Enabled, t.Path = true, ""
	case "false":
		t.Enabled, t.Path = false, ""
	default:
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s path is blank; use -json (stdout) or -json=FILE", "json")
		}
		t.Enabled, t.Path = true, v
	}
	return nil
}

// BareFlagValue implements cliargs.BareValuer: the bare `-json` form
// enables the stream targeting stdout.
func (t *OutTarget) BareFlagValue() string { return "true" }

// Stdout reports whether the enabled target is stdout.
func (t *OutTarget) Stdout() bool { return t.Enabled && t.Path == "" }
