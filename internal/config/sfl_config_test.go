package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestLoadValidSFL(t *testing.T) {
	dir := t.TempDir()
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

func TestLoadRejectsBothSFLOAndOD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\no = \"/a\"\nod = \"/b\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(path, true)
	if err == nil || !strings.Contains(err.Error(), "[sfl] cannot set both o and od") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplySFLResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
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

func TestApplySFLSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\nsecrets = true\nsecrets_path = \"vault/secrets.sqlite\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !f.SFL.Secrets || f.SFL.SecretsPath != "vault/secrets.sqlite" {
		t.Fatalf("unexpected SFL secrets config: %+v", f.SFL)
	}

	secretsOn, secretsPath := false, ""
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{Secrets: &secretsOn, SecretsPath: &secretsPath}); err != nil {
		t.Fatal(err)
	}
	if !secretsOn {
		t.Fatalf("secrets flag not enabled from config")
	}
	if want := filepath.Join(dir, "vault/secrets.sqlite"); secretsPath != want {
		t.Fatalf("secrets-path = %q want %q", secretsPath, want)
	}

	// An explicit CLI -secrets-path wins over the config value.
	cliPath := "/cli/s.sqlite"
	if err := f.ApplySFL(config.Visited{"secrets-path": true}, config.SFLFlags{SecretsPath: &cliPath}); err != nil {
		t.Fatal(err)
	}
	if cliPath != "/cli/s.sqlite" {
		t.Fatalf("secrets-path = %q, want CLI value preserved", cliPath)
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
