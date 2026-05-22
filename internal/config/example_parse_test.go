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

	// uncomment key=value lines, skip mutually-exclusive alternatives
	skipKeys := map[string]bool{
		"o": true, // mutex w/ [sfu].od, example shows both
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

	if _, err := config.Load(dst, true); err != nil {
		t.Fatalf("uncommented example failed to parse: %v\n\n--- generated ---\n%s", err, out.String())
	}
}
