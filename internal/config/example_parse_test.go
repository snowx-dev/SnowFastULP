package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

// uncomment every key in config.toml.example, catches typos/drift
func TestExampleConfigParsesWhenUncommented(t *testing.T) {
	repo := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(repo, "config.toml.example"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}

	// Uncomment key=value lines, skipping only the custom-parser alternative
	// that conflicts with parse_delims. Output keys are allowed to coexist;
	// ApplySFU/ApplySFL define their precedence.
	skipKeys := map[string]bool{
		"parse_rules": true, // mutex w/ [sfu].parse_delims
	}
	keyLine := regexp.MustCompile(`^(\s*)#\s*([A-Za-z_][A-Za-z0-9_]*)(\s*=)`)
	var out strings.Builder
	for _, ln := range strings.Split(string(raw), "\n") {
		if m := keyLine.FindStringSubmatch(ln); m != nil && !skipKeys[m[2]] {
			out.WriteString(m[1] + m[2] + m[3])
			out.WriteString(ln[len(m[0]):])
			out.WriteByte('\n')
			continue
		}
		out.WriteString(ln)
		out.WriteByte('\n')
	}

	tmp := t.TempDir()
	dst := filepath.Join(tmp, "config.toml")
	if err := os.WriteFile(dst, []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := config.Load(dst, true)
	if err != nil {
		t.Fatalf("uncommented example failed to parse: %v\n\n--- generated ---\n%s", err, out.String())
	}
	// The trimmed template ships both custom-parser keys empty (details in
	// the online docs): uncommenting must leave the built-in parser in place.
	if loaded.SFU.ParseDelims != "" || loaded.SFU.ParseRules != "" {
		t.Fatalf("generated example must ship empty custom-parser keys: delims=%q rules=%q",
			loaded.SFU.ParseDelims, loaded.SFU.ParseRules)
	}
	if loaded.History.Enabled == nil || *loaded.History.Enabled {
		t.Fatalf("uncommented example history.enabled = %v, want explicit false", loaded.History.Enabled)
	}
	if loaded.History.Path != "" {
		t.Fatalf("uncommented example history.path = %q, want empty", loaded.History.Path)
	}
}
