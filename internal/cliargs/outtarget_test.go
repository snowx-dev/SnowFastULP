package cliargs_test

import (
	"flag"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
)

// The bare form works through SplitPositional's BareValuer synthesis: the
// splitter rewrites a valueless `-json` into `-json=true`, so the
// flag package never sees a valueless flag.
func TestOutTargetBareFlagMeansStdout(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	flags, pos := cliargs.SplitPositional([]string{"-json"}, fs)
	if len(pos) != 0 {
		t.Fatalf("positionals = %v, want none", pos)
	}
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("bare -json must parse without a value: %v", err)
	}
	if !v.Enabled {
		t.Fatal("bare -json must enable the stream")
	}
	if v.Path != "" {
		t.Fatalf("bare -json path = %q, want empty (stdout)", v.Path)
	}
	if !v.Stdout() {
		t.Fatal("bare -json must target stdout")
	}
}

// The space form (-json FILE) consumes FILE as the flag value — the
// space-form trap fix: FILE must never fall through into the positional
// arguments, where it would become the input path (or trip the arity check).
func TestOutTargetSpaceFormTakesFile(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	flags, pos := cliargs.SplitPositional([]string{"-json", "stats.jsonl"}, fs)
	if len(pos) != 0 {
		t.Fatalf("positionals = %v, want none (FILE is the stream target)", pos)
	}
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("-json FILE: %v", err)
	}
	if !v.Enabled || v.Path != "stats.jsonl" {
		t.Fatalf("-json stats.jsonl = enabled=%v path=%q, want file stats.jsonl", v.Enabled, v.Path)
	}
	if v.Stdout() {
		t.Fatal("the space form with a path must not target stdout")
	}
}

// A flag-shaped token after -json is never swallowed as a value:
// "-json -no-tui in.zip" means stdout + -no-tui + input, not a file
// named "-no-tui".
func TestOutTargetSpaceFormBeforeFlagMeansStdout(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	fs.Bool("no-tui", false, "")
	flags, pos := cliargs.SplitPositional([]string{"-json", "-no-tui", "in.zip"}, fs)
	if len(pos) != 1 || pos[0] != "in.zip" {
		t.Fatalf("positionals = %v, want [in.zip]", pos)
	}
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.Enabled || !v.Stdout() {
		t.Fatalf("-json -no-tui = enabled=%v path=%q, want bare stdout", v.Enabled, v.Path)
	}
}

// The bare form mid-argv (a flag follows) still means stdout.
func TestOutTargetBareMidArgvMeansStdout(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	fs.Bool("no-tui", false, "")
	flags, _ := cliargs.SplitPositional([]string{"-json", "-no-tui"}, fs)
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.Enabled || !v.Stdout() {
		t.Fatalf("-json before another flag = enabled=%v path=%q, want stdout", v.Enabled, v.Path)
	}
}

// The bare form at the end of argv (nothing after it) still means stdout.
func TestOutTargetBareAtEndMeansStdout(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	flags, pos := cliargs.SplitPositional([]string{"in.zip", "-json"}, fs)
	if len(pos) != 1 || pos[0] != "in.zip" {
		t.Fatalf("positionals = %v, want [in.zip]", pos)
	}
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("trailing bare -json must parse: %v", err)
	}
	if !v.Enabled || !v.Stdout() {
		t.Fatalf("trailing bare -json = %+v, want stdout", v)
	}
}

// A plain bool flag following the space form is not consumed either way.
func TestOutTargetSpaceFormThenEqualsForm(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	flags, _ := cliargs.SplitPositional([]string{"-json", "a.jsonl", "-json=-"}, fs)
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !v.Stdout() {
		t.Fatalf("last -json=- must win, got %+v", v)
	}
}

func TestOutTargetEqualsFormTakesFile(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	if err := fs.Parse([]string{"-json=/tmp/stats.jsonl"}); err != nil {
		t.Fatalf("-json=FILE: %v", err)
	}
	if !v.Enabled || v.Path != "/tmp/stats.jsonl" {
		t.Fatalf("got enabled=%v path=%q, want file /tmp/stats.jsonl", v.Enabled, v.Path)
	}
	if v.Stdout() {
		t.Fatal("a path must not target stdout")
	}
}

func TestOutTargetUnsetMeansOff(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if v.Enabled {
		t.Fatal("default must be off")
	}
	if v.Stdout() {
		t.Fatal("off must not look like stdout")
	}
}

func TestOutTargetFalseDisables(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	if err := fs.Parse([]string{"-json=false"}); err != nil {
		t.Fatal(err)
	}
	if v.Enabled {
		t.Fatal("-json=false must disable the stream")
	}
}

func TestOutTargetDashMeansStdout(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	if err := fs.Parse([]string{"-json=-"}); err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || !v.Stdout() {
		t.Fatalf("-json=- must mean stdout, got enabled=%v stdout=%v", v.Enabled, v.Stdout())
	}
}

// Config users spell the stdout target as the word "stdout" (config files
// cannot pass a bare flag); it must behave exactly like "-".
func TestOutTargetStdoutWordMeansStdout(t *testing.T) {
	var v cliargs.OutTarget
	if err := v.Set("stdout"); err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || !v.Stdout() {
		t.Fatalf("json_out = \"stdout\" must mean stdout, got enabled=%v stdout=%v", v.Enabled, v.Stdout())
	}
}

func TestOutTargetRejectsEmptyPath(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	// " " is a whitespace path: enabled but not stdout — reject at parse time.
	if err := fs.Parse([]string{"-json= "}); err == nil {
		t.Fatal("blank path must be a parse error")
	}
}

// file: is the explicit literal-path escape for reserved control spellings
// ("true", "false", "-", "stdout"): only file: forces the literal name.
func TestOutTargetFileEscapeCreatesLiteralNames(t *testing.T) {
	for _, tc := range []struct{ in, wantPath string }{
		{"file:stdout", "stdout"},
		{"file:true", "true"},
		{"file:false", "false"},
		{"file:-", "-"},
		{"file:file:x", "file:x"}, // exactly one prefix is stripped
		{"file:stats.jsonl", "stats.jsonl"},
	} {
		var v cliargs.OutTarget
		if err := v.Set(tc.in); err != nil {
			t.Fatalf("Set(%q): %v", tc.in, err)
		}
		if !v.Enabled || v.Path != tc.wantPath || !v.Literal {
			t.Fatalf("Set(%q) = enabled=%v path=%q literal=%v, want literal file %q", tc.in, v.Enabled, v.Path, v.Literal, tc.wantPath)
		}
		if tc.wantPath == "stdout" && v.Stdout() {
			t.Fatalf("file:stdout must be a literal file, not stdout")
		}
	}
}

func TestOutTargetFileEscapeRejectsEmpty(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	if err := fs.Parse([]string{"-json=file:"}); err == nil {
		t.Fatal("empty file: escape must be a parse error")
	}
}

// Control spellings keep their meanings even though file: exists.
func TestOutTargetControlSpellingsUnchanged(t *testing.T) {
	for _, raw := range []string{"", "true", "-", "stdout"} {
		var v cliargs.OutTarget
		if err := v.Set(raw); err != nil {
			t.Fatalf("Set(%q): %v", raw, err)
		}
		if !v.Enabled || !v.Stdout() {
			t.Fatalf("Set(%q) = %+v, want stdout", raw, v)
		}
	}
	var v cliargs.OutTarget
	if err := v.Set("false"); err != nil {
		t.Fatal(err)
	}
	if v.Enabled {
		t.Fatalf("Set(\"false\") = %+v, want disabled", v)
	}
}
