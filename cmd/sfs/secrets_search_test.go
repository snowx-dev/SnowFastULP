package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"
)

func TestResolveSecretsDBPath(t *testing.T) {
	if got := resolveSecretsDBPath("/tmp/custom.sqlite", "/root"); got != "/tmp/custom.sqlite" {
		t.Fatalf("explicit flag should win, got %q", got)
	}
	if got := resolveSecretsDBPath("", "/data"); got != filepath.Join("/data", secretsDBName) {
		t.Fatalf("root default wrong: %q", got)
	}
	if got := resolveSecretsDBPath("", ""); got != filepath.Join(".", secretsDBName) {
		t.Fatalf("empty root should fall back to CWD: %q", got)
	}
}

func TestBuildSecretsQueryOpts(t *testing.T) {
	// "*" clears the type filter (match all).
	if o, _ := buildSecretsQueryOpts(secretsSearchArgs{pattern: "*", limit: 5}); o.Type != "" || o.Limit != 5 {
		t.Fatalf(`"*" should mean all rows: %+v`, o)
	}
	if o, _ := buildSecretsQueryOpts(secretsSearchArgs{pattern: "aws"}); o.Type != "aws" {
		t.Fatalf("type filter not carried: %+v", o)
	}
	if o, err := buildSecretsQueryOpts(secretsSearchArgs{pattern: "*", since: "1h"}); err != nil || o.Since.IsZero() {
		t.Fatalf("since not parsed: %+v err=%v", o, err)
	}
	if _, err := buildSecretsQueryOpts(secretsSearchArgs{pattern: "*", since: "bogus"}); err == nil {
		t.Fatal("expected an error for an unparseable -since")
	}
}

func TestFormatSecretLine(t *testing.T) {
	line := formatSecretLine(secrets.Match{RuleName: "AWS Access Key", Secret: "AKIA", SourcePath: "log.zip!x.env"})
	if line != "AWS Access Key\tAKIA\tlog.zip!x.env" {
		t.Fatalf("unexpected line: %q", line)
	}
}

func TestCheckSecretsFlagsRejectsStatsAndTxt(t *testing.T) {
	if _, err := checkSecretsFlags(secretsFlagCheck{Stats: true}); err == nil {
		t.Fatal("expected error for -stats with -sec")
	}
	if _, err := checkSecretsFlags(secretsFlagCheck{Txt: true}); err == nil {
		t.Fatal("expected error for -txt with -sec")
	}
	warns, err := checkSecretsFlags(secretsFlagCheck{WorkersSet: true, DecodeStepSet: true, MaxHitsChunkSet: true})
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if len(warns) != 3 {
		t.Fatalf("want 3 notes, got %v", warns)
	}
}

// Config-derived flag values must not trip -sec: when [sfs].stats=true or
// [sfs].workers=4 are in the config but the user did not pass -stats/-j on the
// CLI, visited[...] is false and checkSecretsFlags stays silent. Only an
// explicit CLI flag is treated as user intent.
func TestCheckSecretsFlagsSilentOnConfigDerivedValues(t *testing.T) {
	warns, err := checkSecretsFlags(secretsFlagCheck{})
	if err != nil {
		t.Fatalf("config-only values must not hard-error: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("config-only values must not warn, got %v", warns)
	}
}

// An explicit -stats=false / -txt=false on the CLI is a defensive override of
// config, not an intent to enable the mode. The visited flag is set but the
// merged value is false, so -sec must not reject the run.
func TestCheckSecretsFlagsAllowsExplicitStatsFalse(t *testing.T) {
	if _, err := checkSecretsFlags(secretsFlagCheck{Stats: false}); err != nil {
		t.Fatalf("explicit -stats=false with -sec must not error: %v", err)
	}
}

func TestCheckSecretsFlagsAllowsExplicitTxtFalse(t *testing.T) {
	if _, err := checkSecretsFlags(secretsFlagCheck{Txt: false}); err != nil {
		t.Fatalf("explicit -txt=false with -sec must not error: %v", err)
	}
}

func seedSecretsDB(t *testing.T, dir string) string {
	t.Helper()
	dbPath := filepath.Join(dir, secretsDBName)
	st, err := secrets.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Add(secrets.Finding{RuleID: "aws-access-key", RuleName: "AWS Access Key",
		Secret: "AKIAIOSFODNN7EXAMPLE", Score: -1, SourcePath: "log.zip!config.env"})
	st.Add(secrets.Finding{RuleID: "github-pat", RuleName: "GitHub PAT",
		Secret: "ghp_1234567890abcdefghijklmnopqrstuvwx12", Score: -1})
	if _, err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath
}

func TestRunSecretsSearchFiltersToFileOnly(t *testing.T) {
	dir := t.TempDir()
	seedSecretsDB(t, dir)
	outFile := filepath.Join(dir, "out.txt")

	stdout := captureStdout(t, func() {
		if err := runSecretsSearch(secretsSearchArgs{root: dir, pattern: "aws", outFile: outFile}); err != nil {
			t.Errorf("run: %v", err)
		}
	})
	if strings.Contains(stdout, "AKIA") {
		t.Fatalf("-sec -o must be file-only; stdout leaked secret: %q", stdout)
	}
	body, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 aws line, got %d: %q", len(lines), body)
	}
	if !strings.Contains(lines[0], "AWS Access Key") || !strings.Contains(lines[0], "AKIA") {
		t.Fatalf("unexpected aws line: %q", lines[0])
	}
}

func TestRunSecretsSearchAllToFile(t *testing.T) {
	dir := t.TempDir()
	seedSecretsDB(t, dir)
	outFile := filepath.Join(dir, "all.txt")

	if err := runSecretsSearch(secretsSearchArgs{root: dir, pattern: "*", outFile: outFile}); err != nil {
		t.Fatalf("run: %v", err)
	}
	body, _ := os.ReadFile(outFile)
	if got := strings.Count(strings.TrimSpace(string(body)), "\n"); got != 1 { // 2 lines => 1 separator
		t.Fatalf("want 2 rows for '*', got body: %q", body)
	}
}

func TestRunSecretsSearchMissingDBErrors(t *testing.T) {
	dir := t.TempDir() // no DB seeded
	err := runSecretsSearch(secretsSearchArgs{root: dir, pattern: "*", outFile: filepath.Join(dir, "out.txt")})
	if err == nil {
		t.Fatal("expected an error when the secrets DB is absent")
	}
}
