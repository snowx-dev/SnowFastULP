package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestLoadValidSFL(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.toml")
	content := `
[sfl]
input = "logs"
od = "library"
p = "passwords.txt"
workers = 3
no_tui = true
no_uri = true
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	input, err := f.ResolvedSFLDir("input")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "logs"); input != want {
		t.Fatalf("input = %q want %q", input, want)
	}
	od, err := f.ResolvedSFLDir("od")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "library"); od != want {
		t.Fatalf("od = %q want %q", od, want)
	}
	if f.SFL.Workers == nil || *f.SFL.Workers != 3 || !f.SFL.NoTUI || !f.SFL.NoURI {
		t.Fatalf("unexpected SFL config: %+v", f.SFL)
	}
}

func TestLoadAcceptsBothSFLOAndOD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\no = \"/a\"\nod = \"/b\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path, true); err != nil {
		t.Fatalf("Load rejected both o and od: %v", err)
	}
}

// config sets both o and od and no CLI output flag is given: -od wins, -o is
// ignored (library mode priority). Mirrors the sfu behavior.
func TestApplySFLConfigODTakesPriorityOverO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\no = \"/a\"\nod = \"/b\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	o, od := "", ""
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{O: &o, OD: &od}); err != nil {
		t.Fatalf("ApplySFL returned error: %v", err)
	}
	if od != "/b" {
		t.Fatalf("od = %q, want /b (priority)", od)
	}
	if o != "" {
		t.Fatalf("o = %q, want empty (ignored when od wins)", o)
	}
}

func TestApplySFLResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(filepath.Join(dir, "pw.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[sfl]\no = \"out\"\ntemp_dir = \"tmp\"\np = \"pw.txt\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	o, od, tempDir, password := "", "", "", ""
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{
		O: &o, OD: &od, TempDir: &tempDir, Password: &password,
	}); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "out"); o != want {
		t.Fatalf("o = %q want %q", o, want)
	}
	if want := filepath.Join(dir, "tmp"); tempDir != want {
		t.Fatalf("temp-dir = %q want %q", tempDir, want)
	}
	if want := filepath.Join(dir, "pw.txt"); password != want {
		t.Fatalf("p = %q want %q", password, want)
	}
}

func TestApplySFLCLIOOverridesConfigOD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\nod = \"lib\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	o, od := "/cli/out", ""
	if err := f.ApplySFL(config.Visited{"o": true}, config.SFLFlags{O: &o, OD: &od}); err != nil {
		t.Fatalf("ApplySFL returned error: %v", err)
	}
	if o != "/cli/out" {
		t.Fatalf("o = %q, want CLI value", o)
	}
	if od != "" {
		t.Fatalf("od = %q, want config value ignored", od)
	}
}

// [sfl] odr = true reuses the od path: ApplySFL resolves od into the OD flag
// and sets ODR=true so the CLI flips dry-run on a -od run. The CLI -odr path
// suppresses the config od pull so the two don't trip mutual exclusion.
func TestApplySFLODRReusesODPath(t *testing.T) {
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
	if !f.SFL.ODR {
		t.Fatalf("SFL.ODR = false, want true")
	}

	od, odr := "", false
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{OD: &od, ODR: &odr}); err != nil {
		t.Fatalf("ApplySFL: %v", err)
	}
	if want := filepath.Join(dir, "lib"); od != want {
		t.Fatalf("od = %q, want resolved %q", od, want)
	}
	if !odr {
		t.Fatalf("odr flag not enabled from config")
	}

	// CLI -odr suppresses the config od pull so mutual exclusion in main()
	// doesn't see both od and odr populated from config.
	od2, odr2 := "", false
	if err := f.ApplySFL(config.Visited{"odr": true}, config.SFLFlags{OD: &od2, ODR: &odr2}); err != nil {
		t.Fatalf("ApplySFL with -odr: %v", err)
	}
	if od2 != "" {
		t.Fatalf("config od should NOT be pulled when -odr is on the CLI; od = %q", od2)
	}
}

func TestApplySFLEnvFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\nenv = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	env := false
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{Env: &env}); err != nil {
		t.Fatal(err)
	}
	if !env {
		t.Fatal("expected env=true from config")
	}
}

func TestApplySFLDebugRejectFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\ndebug_reject = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	debugReject := false
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{DebugReject: &debugReject}); err != nil {
		t.Fatalf("ApplySFL returned error: %v", err)
	}
	if !debugReject {
		t.Fatal("expected debug_reject=true from config")
	}
}

func TestApplySFLCLIDebugRejectWinsOverConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\ndebug_reject = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	// CLI -debug-reject was set: config must not flip it (Visited tracks it).
	debugReject := true
	if err := f.ApplySFL(config.Visited{"debug-reject": true}, config.SFLFlags{DebugReject: &debugReject}); err != nil {
		t.Fatalf("ApplySFL returned error: %v", err)
	}
	if !debugReject {
		t.Fatal("CLI -debug-reject must stay true; config must not unset it")
	}
	// And a config-only load with an unrelated CLI flag still applies.
	debugReject = false
	if err := f.ApplySFL(config.Visited{"debug": true}, config.SFLFlags{DebugReject: &debugReject}); err != nil {
		t.Fatalf("ApplySFL returned error: %v", err)
	}
	if !debugReject {
		t.Fatal("config debug_reject must apply when -debug-reject is not on the CLI")
	}
}
