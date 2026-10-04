package config_test

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func writeConfig(t *testing.T, body string) config.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestApplySFUJSONOutConfigPull(t *testing.T) {
	f := writeConfig(t, "[sfu]\njson_out = \"/tmp/stats.jsonl\"\njson_every = \"2s\"\n")
	var target cliargs.OutTarget
	every := 800 * time.Millisecond
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &target, JSONEvery: &every}); err != nil {
		t.Fatal(err)
	}
	if !target.Enabled || target.Path != "/tmp/stats.jsonl" {
		t.Fatalf("target = %+v, want file /tmp/stats.jsonl", target)
	}
	if every != 2*time.Second {
		t.Fatalf("every = %v, want 2s", every)
	}
}

func TestApplySFUJSONOutConfigStdout(t *testing.T) {
	f := writeConfig(t, "[sfu]\njson_out = \"-\"\n")
	var target cliargs.OutTarget
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	if !target.Enabled || !target.Stdout() {
		t.Fatalf("target = %+v, want stdout", target)
	}
}

func TestApplySFUJSONOutCLIWins(t *testing.T) {
	f := writeConfig(t, "[sfu]\njson_out = \"/from/config.jsonl\"\n")
	var target cliargs.OutTarget
	if err := target.Set("/from/cli.jsonl"); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{"json": true}
	if err := f.ApplySFU(visited, config.SFUFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	if target.Path != "/from/cli.jsonl" {
		t.Fatalf("target = %+v, want the CLI value kept", target)
	}
}

func TestApplySFUJSONOutBadEveryIsAnError(t *testing.T) {
	f := writeConfig(t, "[sfu]\njson_every = \"soon\"\n")
	var target cliargs.OutTarget
	every := 800 * time.Millisecond
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &target, JSONEvery: &every}); err == nil {
		t.Fatal("unparseable json_every must error")
	}
}

func TestApplySFLJSONOutConfigPull(t *testing.T) {
	f := writeConfig(t, "[sfl]\njson_out = \"stats.jsonl\"\njson_every = \"1s\"\n")
	var target cliargs.OutTarget
	every := 800 * time.Millisecond
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{JSONOut: &target, JSONEvery: &every}); err != nil {
		t.Fatal(err)
	}
	// Relative config paths resolve against the process CWD (ResolvePath).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "stats.jsonl"); !target.Enabled || target.Path != want {
		t.Fatalf("target = %+v, want file %s", target, want)
	}
	if every != time.Second {
		t.Fatalf("every = %v, want 1s", every)
	}
}

func TestApplySFLJSONOutCLIWins(t *testing.T) {
	f := writeConfig(t, "[sfl]\njson_out = \"/from/config.jsonl\"\n")
	var target cliargs.OutTarget
	if err := target.Set(""); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{"json": true}
	if err := f.ApplySFL(visited, config.SFLFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	if target.Path != "" || !target.Stdout() {
		t.Fatalf("target = %+v, want the bare CLI stdout target kept", target)
	}
}

// setHomeEnv points os.UserHomeDir at home on every platform.
func setHomeEnv(t *testing.T, home string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
}

// Config json_out is a real path like every other config path: ~ expands to
// the home directory.
func TestApplySFUJSONOutConfigResolvesTilde(t *testing.T) {
	home := t.TempDir()
	setHomeEnv(t, home)
	f := writeConfig(t, "[sfu]\njson_out = \"~/stats.jsonl\"\n")
	var target cliargs.OutTarget
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "stats.jsonl")
	if !target.Enabled || target.Path != want {
		t.Fatalf("target = enabled=%v path=%q, want %q", target.Enabled, target.Path, want)
	}
}

// Relative config json_out resolves against the process CWD, matching
// ResolvePath semantics for all other config paths.
func TestApplySFUJSONOutConfigResolvesRelativeAgainstCWD(t *testing.T) {
	f := writeConfig(t, "[sfu]\njson_out = \"stats.jsonl\"\n")
	var target cliargs.OutTarget
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, "stats.jsonl")
	if !target.Enabled || target.Path != want {
		t.Fatalf("target = enabled=%v path=%q, want %q", target.Enabled, target.Path, want)
	}
}

// Control spellings keep their meanings through the config merge.
func TestApplySFUJSONOutConfigControlTokensUnchanged(t *testing.T) {
	for _, raw := range []string{"-", "stdout", "true"} {
		f := writeConfig(t, "[sfu]\njson_out = \""+raw+"\"\n")
		var target cliargs.OutTarget
		if err := f.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &target}); err != nil {
			t.Fatalf("json_out = %q: %v", raw, err)
		}
		if !target.Stdout() {
			t.Fatalf("json_out = %q: target = %+v, want stdout", raw, target)
		}
	}
	fFalse := writeConfig(t, "[sfu]\njson_out = \"false\"\n")
	var off cliargs.OutTarget
	if err := fFalse.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &off}); err != nil {
		t.Fatal(err)
	}
	if off.Enabled {
		t.Fatalf("json_out = \"false\": target = %+v, want disabled", off)
	}
}

// The file: escape passes through the config merge untouched: json_out =
// "file:stdout" creates a literal file named stdout, not a stdout stream.
func TestApplySFUJSONOutConfigFileEscape(t *testing.T) {
	f := writeConfig(t, "[sfu]\njson_out = \"file:stdout\"\n")
	var target cliargs.OutTarget
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	if !target.Enabled || target.Path != "stdout" || target.Stdout() {
		t.Fatalf("target = %+v, want literal file named stdout", target)
	}
}

// -json=false keeps disabling the stream (bare bool flag behavior).
func TestOutTargetFlagFalseStillDisables(t *testing.T) {
	var v cliargs.OutTarget
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Var(&v, "json", "")
	if err := fs.Parse([]string{"-json=false"}); err != nil {
		t.Fatal(err)
	}
	if v.Enabled || v.Stdout() {
		t.Fatalf("-json=false = %+v, want fully disabled", v)
	}
}

// The sfl merge resolves json_out paths the same way.
func TestApplySFLJSONOutConfigResolvesTilde(t *testing.T) {
	home := t.TempDir()
	setHomeEnv(t, home)
	f := writeConfig(t, "[sfl]\njson_out = \"~/stats.jsonl\"\n")
	var target cliargs.OutTarget
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "stats.jsonl")
	if !target.Enabled || target.Path != want {
		t.Fatalf("target = enabled=%v path=%q, want %q", target.Enabled, target.Path, want)
	}
}

func TestApplySFSJSONOutConfigPull(t *testing.T) {
	f := writeConfig(t, "[sfs]\njson_out = \"/tmp/stats.jsonl\"\njson_every = \"2s\"\n")
	var target cliargs.OutTarget
	every := 800 * time.Millisecond
	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{JSONOut: &target, JSONEvery: &every}); err != nil {
		t.Fatal(err)
	}
	if !target.Enabled || target.Path != "/tmp/stats.jsonl" {
		t.Fatalf("target = %+v, want file /tmp/stats.jsonl", target)
	}
	if every != 2*time.Second {
		t.Fatalf("every = %v, want 2s", every)
	}
}

func TestApplySFSJSONOutConfigStdout(t *testing.T) {
	f := writeConfig(t, "[sfs]\njson_out = \"-\"\n")
	var target cliargs.OutTarget
	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	if !target.Enabled || !target.Stdout() {
		t.Fatalf("target = %+v, want stdout", target)
	}
}

func TestApplySFSJSONOutCLIWins(t *testing.T) {
	f := writeConfig(t, "[sfs]\njson_out = \"/from/config.jsonl\"\n")
	var target cliargs.OutTarget
	if err := target.Set("/from/cli.jsonl"); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{"json": true}
	if err := f.ApplySFS(visited, config.SFSFlags{JSONOut: &target}); err != nil {
		t.Fatal(err)
	}
	if target.Path != "/from/cli.jsonl" {
		t.Fatalf("target = %+v, want the CLI value kept", target)
	}
}

func TestApplySFSJSONOutBadEveryIsAnError(t *testing.T) {
	f := writeConfig(t, "[sfs]\njson_every = \"soon\"\n")
	var target cliargs.OutTarget
	every := 800 * time.Millisecond
	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{JSONOut: &target, JSONEvery: &every}); err == nil {
		t.Fatal("unparseable json_every must error")
	}
}
