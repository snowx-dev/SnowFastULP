package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestLoadMissingFileNotExplicit(t *testing.T) {
	f, err := config.Load(filepath.Join(t.TempDir(), "nope.toml"), false)
	if err != nil {
		t.Fatal(err)
	}
	if f.SFS.Dir != "" || f.SFU.OD != "" {
		t.Fatalf("expected zero file, got %+v", f)
	}
}

func TestLoadValidSFUAndSFS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[sfu]
od = "lib"
zst = true

[sfs]
dir = "lib"
clean = true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	od, err := f.ResolvedSFUDir("od")
	if err != nil || od != filepath.Join(dir, "lib") {
		t.Fatalf("od = %q err %v", od, err)
	}
	sfsDir, err := f.ResolvedSFSDir()
	if err != nil || sfsDir != filepath.Join(dir, "lib") {
		t.Fatalf("dir = %q err %v", sfsDir, err)
	}
	if !f.SFU.Zst || !f.SFS.Clean {
		t.Fatalf("bools: sfu zst=%v sfs clean=%v", f.SFU.Zst, f.SFS.Clean)
	}
}

func TestLoadRejectsBothOAndOD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\no = \"/a\"\nod = \"/b\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path, true)
	if err == nil || !strings.Contains(err.Error(), "both o and od") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultPathUnixStyle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix path test")
	}
	p, err := config.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, string(filepath.Separator)+".config"+string(filepath.Separator)) {
		t.Fatalf("path = %q", p)
	}
	if !strings.HasSuffix(p, "snowfast"+string(filepath.Separator)+"config.toml") {
		t.Fatalf("path = %q", p)
	}
}

func TestStripConfigArgv(t *testing.T) {
	got := config.StripConfigArgv([]string{"-config", "/tmp/c.toml", "-silent", "pat"})
	want := []string{"-silent", "pat"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	got = config.StripConfigArgv([]string{"--config=/etc/snow.toml", "x"})
	if len(got) != 1 || got[0] != "x" {
		t.Fatalf("got %v", got)
	}
}

func TestPathFromArgv(t *testing.T) {
	p, ex := config.PathFromArgv([]string{"-config", "/tmp/c.toml", "x"})
	if !ex || p != "/tmp/c.toml" {
		t.Fatalf("got %q %v", p, ex)
	}
	p, ex = config.PathFromArgv([]string{"--config=/etc/snow.toml"})
	if !ex || p != "/etc/snow.toml" {
		t.Fatalf("got %q %v", p, ex)
	}
}

func TestStripConfigArgvRejectsDashValue(t *testing.T) {
	// must not eat -silent as path, mirror flag.Parse missing-value behavior
	got := config.StripConfigArgv([]string{"-config", "-silent", "pat"})
	want := []string{"-config", "-silent", "pat"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestPathFromArgvRejectsDashValue(t *testing.T) {
	p, ex := config.PathFromArgv([]string{"-config", "-silent", "pat"})
	if ex || p != "" {
		t.Fatalf("got %q %v; expected ('' false)", p, ex)
	}
	// bare "-" is stdin sentinel, not a flag
	p, ex = config.PathFromArgv([]string{"-config", "-"})
	if !ex || p != "-" {
		t.Fatalf("bare-dash: got %q %v; expected ('-' true)", p, ex)
	}
}

func TestResolvePathExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/foo", filepath.Join(home, "foo")},
		{"~/a/b/c", filepath.Join(home, "a", "b", "c")},
	}
	for _, c := range cases {
		got, err := config.ResolvePath("/some/base", c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q -> %q want %q", c.in, got, c.want)
		}
	}
}

func TestResolveConfigPathArgvBeatsEnv(t *testing.T) {
	t.Setenv("SNOWFAST_CONFIG", "/from/env.toml")
	p, ex, err := config.ResolveConfigPath([]string{"-config", "/from/argv.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if !ex || p != filepath.Clean("/from/argv.toml") {
		t.Fatalf("got %q ex=%v", p, ex)
	}
}

func TestResolveConfigPathEnvBeatsDefault(t *testing.T) {
	t.Setenv("SNOWFAST_CONFIG", "/from/env.toml")
	p, ex, err := config.ResolveConfigPath([]string{"pat"})
	if err != nil {
		t.Fatal(err)
	}
	if !ex || p != filepath.Clean("/from/env.toml") {
		t.Fatalf("got %q ex=%v", p, ex)
	}
}

func TestResolveConfigPathFallsBackToDefault(t *testing.T) {
	t.Setenv("SNOWFAST_CONFIG", "")
	p, ex, err := config.ResolveConfigPath([]string{"pat"})
	if err != nil {
		t.Fatal(err)
	}
	if ex {
		t.Fatalf("expected explicit=false; got %v", ex)
	}
	if !strings.HasSuffix(p, filepath.Join("snowfast", "config.toml")) {
		t.Fatalf("default path %q lacks snowfast/config.toml suffix", p)
	}
}

func TestResolvedSFUDirRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\nod = \"lib\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ResolvedSFUDir("typo"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestApplySFURejectsCLIOWithConfigOD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\nod = \"lib\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	o, od := "/cli/out", ""
	v := config.Visited{"o": true}
	err = f.ApplySFU(v, config.SFUFlags{O: &o, OD: &od})
	if err == nil || !strings.Contains(err.Error(), "[sfu].od") {
		t.Fatalf("expected -o vs [sfu].od conflict; got %v", err)
	}
}

func TestApplySFURejectsCLIODWithConfigO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\no = \"out\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	o, od := "", "/cli/lib"
	v := config.Visited{"od": true}
	err = f.ApplySFU(v, config.SFUFlags{O: &o, OD: &od})
	if err == nil || !strings.Contains(err.Error(), "[sfu].o") {
		t.Fatalf("expected -od vs [sfu].o conflict; got %v", err)
	}
}

func TestApplySFUResolvesRelativeODAgainstBaseDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\nod = \"lib\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	o, od := "", ""
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{O: &o, OD: &od}); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "lib"); od != want {
		t.Fatalf("od = %q want %q", od, want)
	}
}

func TestApplySFUResolvesTempDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\ntemp_dir = \"tmp\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	o, od, td := "", "", ""
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{O: &o, OD: &od, TempDir: &td}); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "tmp"); td != want {
		t.Fatalf("temp-dir = %q want %q", td, want)
	}
}
