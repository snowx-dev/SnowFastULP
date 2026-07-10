package config_test

import (
	"os"
	"path/filepath"
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

// [sfl] odr = true reuses the od path: ApplySFL resolves od into the OD flag
// and sets ODR=true so the CLI flips dry-run on a -od run. The CLI -odr path
// suppresses the config od pull so the two don't trip mutual exclusion.
func TestApplySFLODRReusesODPath(t *testing.T) {
	dir := t.TempDir()
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

// config secrets_allow/deny populate the flag slices when no CLI flag is set.
func TestApplySFLSecretsAllowDenyFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\nsecrets_allow = [\"np.aws.*\"]\nsecrets_deny = [\"np.aws.3\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var allow, deny []string
	if err := f.ApplySFL(config.Visited{}, config.SFLFlags{SecretsAllow: &allow, SecretsDeny: &deny}); err != nil {
		t.Fatal(err)
	}
	if len(allow) != 1 || allow[0] != "np.aws.*" {
		t.Fatalf("allow = %v, want [np.aws.*]", allow)
	}
	if len(deny) != 1 || deny[0] != "np.aws.3" {
		t.Fatalf("deny = %v, want [np.aws.3]", deny)
	}
}

// CLI -secrets-allow / -secrets-deny win over config: the config slices are
// not pulled into the flag pointers when the flag was visited.
func TestApplySFLSecretsAllowCLIOverridesConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfl]\nsecrets_allow = [\"np.aws.*\"]\nsecrets_deny = [\"np.aws.3\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	allow, deny := []string{"np.slack.*"}, []string{}
	if err := f.ApplySFL(config.Visited{"secrets-allow": true, "secrets-deny": true}, config.SFLFlags{SecretsAllow: &allow, SecretsDeny: &deny}); err != nil {
		t.Fatal(err)
	}
	if len(allow) != 1 || allow[0] != "np.slack.*" {
		t.Fatalf("CLI allow lost to config: allow = %v", allow)
	}
	if len(deny) != 0 {
		t.Fatalf("CLI deny (empty) lost to config: deny = %v", deny)
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
