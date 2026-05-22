package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

// [sfu].input resolved against config file dir, not CWD
func TestResolvedSFUInputRelative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[sfu]
input = "./dumps/raw"
od    = "./library"
del   = true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.ResolvedSFUDir("input")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "dumps", "raw")
	if in != want {
		t.Fatalf("input = %q, want %q", in, want)
	}
}

// absolute paths must not be re-rooted under config base
func TestResolvedSFUInputAbsolutePassthrough(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	abs := "/var/lib/dumps"
	content := "[sfu]\ninput = \"" + abs + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.ResolvedSFUDir("input")
	if err != nil {
		t.Fatal(err)
	}
	if in != abs {
		t.Fatalf("input = %q, want %q (absolute passthrough)", in, abs)
	}
}

// absent input key = "" + nil err, callers detect via "" sentinel
// and fall through to CLI-required error
func TestResolvedSFUInputEmptyWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[sfu]\nzst = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	in, err := f.ResolvedSFUDir("input")
	if err != nil {
		t.Fatal(err)
	}
	if in != "" {
		t.Fatalf("input = %q, want empty", in)
	}
}
