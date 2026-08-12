package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

// [sfu].parse_delims / parse_rules land on flags when no CLI flag set them.
func TestSFUParseCustomFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := `
[sfu]
parse_delims = "|"
parse_rules  = "./rules.txt"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	var delims, rules string
	err = f.ApplySFU(config.NewVisited(), config.SFUFlags{ParseDelims: &delims, ParseRules: &rules})
	if err != nil {
		t.Fatal(err)
	}
	if delims != "|" {
		t.Fatalf("parse_delims = %q", delims)
	}
	// parse_rules is resolved against the config file's dir
	if want := filepath.Join(dir, "rules.txt"); rules != want {
		t.Fatalf("parse_rules = %q, want %q", rules, want)
	}
}

// CLI flag wins over config.
func TestSFUParseCustomCLIBeatsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[sfu]\nparse_delims = \"|\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	delims := ";" // CLI already set
	v := config.Visited{"parse-delims": true}
	if err := f.ApplySFU(v, config.SFUFlags{ParseDelims: &delims}); err != nil {
		t.Fatal(err)
	}
	if delims != ";" {
		t.Fatalf("config overwrote CLI flag: %q", delims)
	}
}

// CLI -parse-delims suppresses config parse_rules (XOR peer), same group as -o/-od/-odr.
func TestSFUParseDelimsCLISuppressesConfigRules(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[sfu]\nparse_rules = \"./rules.txt\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	delims := "|"
	var rules string
	v := config.Visited{"parse-delims": true}
	if err := f.ApplySFU(v, config.SFUFlags{ParseDelims: &delims, ParseRules: &rules}); err != nil {
		t.Fatal(err)
	}
	if delims != "|" {
		t.Fatalf("CLI delims clobbered: %q", delims)
	}
	if rules != "" {
		t.Fatalf("config parse_rules applied under CLI parse-delims: %q", rules)
	}
}

// CLI -parse-rules suppresses config parse_delims.
func TestSFUParseRulesCLISuppressesConfigDelims(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[sfu]\nparse_delims = \"|\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(cfgPath, true)
	if err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(dir, "cli-rules.txt")
	var delims string
	v := config.Visited{"parse-rules": true}
	if err := f.ApplySFU(v, config.SFUFlags{ParseDelims: &delims, ParseRules: &rules}); err != nil {
		t.Fatal(err)
	}
	if rules != filepath.Join(dir, "cli-rules.txt") {
		t.Fatalf("CLI rules clobbered: %q", rules)
	}
	if delims != "" {
		t.Fatalf("config parse_delims applied under CLI parse-rules: %q", delims)
	}
}
