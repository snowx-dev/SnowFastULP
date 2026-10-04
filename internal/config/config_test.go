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
	t.Chdir(dir)
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

func TestLoadAcceptsBothOAndOD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\no = \"/a\"\nod = \"/b\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path, true); err != nil {
		t.Fatalf("Load rejected both o and od: %v", err)
	}
}

// config sets both o and od and no CLI output flag is given: -od wins, -o is
// ignored (library mode priority).
func TestApplySFUConfigODTakesPriorityOverO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\no = \"/a\"\nod = \"/b\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	o, od := "", ""
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{O: &o, OD: &od}); err != nil {
		t.Fatalf("ApplySFU returned error: %v", err)
	}
	if od != "/b" {
		t.Fatalf("od = %q, want /b (priority)", od)
	}
	if o != "" {
		t.Fatalf("o = %q, want empty (ignored when od wins)", o)
	}
}

func TestApplySFUNoFastPathConfigAndCLIOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\nno_fast_path = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	configValue := false
	if err := f.ApplySFU(config.Visited{}, config.SFUFlags{NoFastPath: &configValue}); err != nil {
		t.Fatal(err)
	}
	if !configValue {
		t.Fatal("config no_fast_path=true was not applied")
	}

	cliValue := false
	visited := config.Visited{"no-fast-path": true}
	if err := f.ApplySFU(visited, config.SFUFlags{NoFastPath: &cliValue}); err != nil {
		t.Fatal(err)
	}
	if cliValue {
		t.Fatal("explicit -no-fast-path=false should override config")
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
		got, err := config.ResolvePath(c.in)
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

func TestApplySFUCLIOOverridesConfigOD(t *testing.T) {
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
	if err := f.ApplySFU(v, config.SFUFlags{O: &o, OD: &od}); err != nil {
		t.Fatalf("ApplySFU returned error: %v", err)
	}
	if o != "/cli/out" {
		t.Fatalf("o = %q, want CLI value", o)
	}
	if od != "" {
		t.Fatalf("od = %q, want config value ignored", od)
	}
}

func TestApplySFUCLIODOverridesConfigO(t *testing.T) {
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
	if err := f.ApplySFU(v, config.SFUFlags{O: &o, OD: &od}); err != nil {
		t.Fatalf("ApplySFU returned error: %v", err)
	}
	if o != "" {
		t.Fatalf("o = %q, want config value ignored", o)
	}
	if od != "/cli/lib" {
		t.Fatalf("od = %q, want CLI value", od)
	}
}

func TestApplySFUResolvesRelativeODAgainstCWD(t *testing.T) {
	work := t.TempDir()
	cfgDir := t.TempDir()
	t.Chdir(work)
	path := filepath.Join(cfgDir, "config.toml")
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
	if want := filepath.Join(work, "lib"); od != want {
		t.Fatalf("od = %q want %q (CWD, not config dir)", od, want)
	}
}

func TestApplySFUResolvesTempDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
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

// writeRawConfig writes body to a temp config file and returns its path.
func writeRawConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Typos must fail loudly instead of silently falling back to defaults:
// unknown section members, unknown sections, and unknown top-level keys all
// name the offending dotted key(s).
func TestLoadRejectsUnknownKeys(t *testing.T) {
	t.Run("misspelled section member", func(t *testing.T) {
		_, err := config.Load(writeRawConfig(t, "[sfu]\nwrkers = 4\n"), true)
		if err == nil {
			t.Fatal("misspelled sfu key must error")
		}
		if !strings.Contains(err.Error(), "sfu.wrkers") {
			t.Fatalf("error must name the key, got: %v", err)
		}
	})
	t.Run("unknown section", func(t *testing.T) {
		_, err := config.Load(writeRawConfig(t, "[sfuu]\nworkers = 4\n"), true)
		if err == nil {
			t.Fatal("unknown section must error")
		}
		if !strings.Contains(err.Error(), "sfuu") {
			t.Fatalf("error must name the section, got: %v", err)
		}
	})
	t.Run("unknown top-level key", func(t *testing.T) {
		_, err := config.Load(writeRawConfig(t, "stray = 1\n[sfu]\nworkers = 4\n"), true)
		if err == nil {
			t.Fatal("unknown top-level key must error")
		}
		if !strings.Contains(err.Error(), "stray") {
			t.Fatalf("error must name the key, got: %v", err)
		}
	})
	t.Run("multiple unknown keys sorted", func(t *testing.T) {
		_, err := config.Load(writeRawConfig(t, "zeta = 1\n[sfu]\nalfa = 2\nbeta = 3\n"), true)
		if err == nil {
			t.Fatal("unknown keys must error")
		}
		alfa := strings.Index(err.Error(), "sfu.alfa")
		beta := strings.Index(err.Error(), "sfu.beta")
		if alfa < 0 || beta < 0 || alfa > beta {
			t.Fatalf("error must name sfu.alfa before sfu.beta, got: %v", err)
		}
	})
}

// The shipped example documents exactly the supported key set, so the real
// example (all keys commented) must load with zero unknown keys.
func TestLoadEmbeddedExampleHasNoUnknownKeys(t *testing.T) {
	raw, err := os.ReadFile("example.toml")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "example.toml")
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(dst, true); err != nil {
		t.Fatalf("embedded example must load cleanly: %v", err)
	}
}

// An explicit CLI output flag (-o, -od, or -odr) owns the complete
// output-mode tuple: configured odr=true must NOT leak into the flags, or
// main's output-mode resolution aborts with an odr error even though the
// user explicitly chose a normal -o output. With no CLI output flag, config
// od + odr=true still enables the dry-run (unchanged behavior).
func TestApplySFUODRExplicitCLIOutputSuppressesConfigODR(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfu]\nod = \"lib\"\nodr = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		visited    config.Visited
		wantODR    bool
		wantODPull bool // config od pulled into the OD flag
	}{
		{name: "cli o suppresses config odr", visited: config.Visited{"o": true}, wantODR: false, wantODPull: false},
		{name: "cli od suppresses config odr", visited: config.Visited{"od": true}, wantODR: false, wantODPull: false},
		{name: "cli odr suppresses config odr", visited: config.Visited{"odr": true}, wantODR: false, wantODPull: false},
		{name: "no cli output flag keeps dry-run", visited: config.Visited{}, wantODR: true, wantODPull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, od, odr := "", "", false
			err := f.ApplySFU(tc.visited, config.SFUFlags{O: &o, OD: &od, ODR: &odr})
			if err != nil {
				t.Fatalf("ApplySFU: %v", err)
			}
			if odr != tc.wantODR {
				t.Fatalf("odr flag = %v, want %v", odr, tc.wantODR)
			}
			if tc.wantODPull {
				if want := filepath.Join(dir, "lib"); od != want {
					t.Fatalf("od = %q, want resolved %q", od, want)
				}
			} else if od != "" {
				t.Fatalf("od flag = %q, want empty (CLI owns the output mode)", od)
			}
		})
	}
}

// The sfl merge applies the same CLI-owns-the-tuple rule.
func TestApplySFLODRExplicitCLIOutputSuppressesConfigODR(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\nod = \"lib\"\nodr = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		visited    config.Visited
		wantODR    bool
		wantODPull bool
	}{
		{name: "cli o suppresses config odr", visited: config.Visited{"o": true}, wantODR: false, wantODPull: false},
		{name: "cli od suppresses config odr", visited: config.Visited{"od": true}, wantODR: false, wantODPull: false},
		{name: "cli odr suppresses config odr", visited: config.Visited{"odr": true}, wantODR: false, wantODPull: false},
		{name: "no cli output flag keeps dry-run", visited: config.Visited{}, wantODR: true, wantODPull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, od, odr := "", "", false
			err := f.ApplySFL(tc.visited, config.SFLFlags{O: &o, OD: &od, ODR: &odr})
			if err != nil {
				t.Fatalf("ApplySFL: %v", err)
			}
			if odr != tc.wantODR {
				t.Fatalf("odr flag = %v, want %v", odr, tc.wantODR)
			}
			if tc.wantODPull {
				if want := filepath.Join(dir, "lib"); od != want {
					t.Fatalf("od = %q, want resolved %q", od, want)
				}
			} else if od != "" {
				t.Fatalf("od flag = %q, want empty (CLI owns the output mode)", od)
			}
		})
	}
}
