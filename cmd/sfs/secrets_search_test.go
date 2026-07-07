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

func TestRunSecretsSearchFiltersToFile(t *testing.T) {
	dir := t.TempDir()
	seedSecretsDB(t, dir)
	outFile := filepath.Join(dir, "out.txt")

	if err := runSecretsSearch(secretsSearchArgs{root: dir, pattern: "aws", outFile: outFile}); err != nil {
		t.Fatalf("run: %v", err)
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
