package config_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

// [sfs].sec + secrets_path flow into flags when the CLI didn't set them, and
// secrets_path resolves against the config dir (mirroring sfl).
func TestApplySFSSecretsFromTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[sfs]
sec = true
secrets_path = "vault/secrets.sqlite"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("sfs", flag.ContinueOnError)
	sec := fs.Bool("sec", false, "")
	secretsPath := fs.String("secrets-path", "", "")
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	if err := f.ApplySFS(visited, config.SFSFlags{Sec: sec, SecretsPath: secretsPath}); err != nil {
		t.Fatal(err)
	}
	if !*sec {
		t.Fatal("sec = false, want true (from TOML)")
	}
	if want := filepath.Join(dir, "vault/secrets.sqlite"); *secretsPath != want {
		t.Fatalf("secrets-path = %q, want %q (resolved against config dir)", *secretsPath, want)
	}
}

// Explicit CLI flags override [sfs] config values.
func TestApplySFSSecretsCLIWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[sfs]
sec = true
secrets_path = "vault/secrets.sqlite"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}

	fs := flag.NewFlagSet("sfs", flag.ContinueOnError)
	sec := fs.Bool("sec", false, "")
	secretsPath := fs.String("secrets-path", "", "")
	if err := fs.Parse([]string{"-secrets-path", "/abs/override.sqlite"}); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	if err := f.ApplySFS(visited, config.SFSFlags{Sec: sec, SecretsPath: secretsPath}); err != nil {
		t.Fatal(err)
	}
	if *secretsPath != "/abs/override.sqlite" {
		t.Fatalf("secrets-path = %q, want the CLI value", *secretsPath)
	}
}
