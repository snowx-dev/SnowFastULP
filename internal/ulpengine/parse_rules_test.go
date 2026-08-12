package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRules(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRegexRulesHappyPath(t *testing.T) {
	path := writeRules(t, `^(?P<url>\S+)\|(?P<login>[^|]+)\|(?P<password>.+)$`)
	p, n, err := NewRegexRulesParser(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rule count = %d, want 1", n)
	}
	host, url, login, password, ok := p.Parse("https://example.com/login|user@example.com|s3cret")
	if !ok {
		t.Fatal("matching line rejected")
	}
	if host != "example.com" || url != "https://example.com/login" || login != "user@example.com" || password != "s3cret" {
		t.Fatalf("got host=%q url=%q login=%q password=%q", host, url, login, password)
	}
	if _, _, _, _, ok := p.Parse("not a credential line"); ok {
		t.Fatal("non-matching line accepted")
	}
}

func TestRegexRulesFirstMatchWins(t *testing.T) {
	// Rule 1 only matches lines starting with "CSV:", rule 2 is the pipe shape.
	path := writeRules(t, strings.Join([]string{
		`^CSV:(?P<host>[^,]+),(?P<login>[^,]+),(?P<password>.+)$`,
		`^(?P<url>\S+)\|(?P<login>[^|]+)\|(?P<password>.+)$`,
	}, "\n"))
	p, n, err := NewRegexRulesParser(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rule count = %d, want 2", n)
	}
	host, url, login, password, ok := p.Parse("CSV:example.com,bob,hunter2")
	if !ok {
		t.Fatal("CSV line rejected")
	}
	if host != "example.com" || login != "bob" || password != "hunter2" {
		t.Fatalf("CSV parse got host=%q login=%q password=%q", host, login, password)
	}
	// host-only capture: url falls back to host, then finishParse derives host.
	if url != "example.com" {
		t.Fatalf("url = %q, want host fallback", url)
	}
}

func TestRegexRulesSkipsCommentsAndBlanks(t *testing.T) {
	path := writeRules(t, "# comment\n\n^(?P<url>\\S+);(?P<login>[^;]+);(?P<password>.+)$\n\n")
	_, n, err := NewRegexRulesParser(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rule count = %d, want 1", n)
	}
}

func TestRegexRulesValidationErrors(t *testing.T) {
	cases := map[string]string{
		"missing login":    `^(?P<url>\S+):(?P<password>.+)$`,
		"missing password": `^(?P<url>\S+):(?P<login>.+)$`,
		"missing url/host": `^(?P<login>.+):(?P<password>.+)$`,
		"bad regex":        `^(?P<url>\S+$`,
	}
	for name, content := range cases {
		if _, _, err := NewRegexRulesParser(writeRules(t, content)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestRegexRulesEmptyFile(t *testing.T) {
	if _, _, err := NewRegexRulesParser(writeRules(t, "# only a comment\n\n")); err == nil {
		t.Fatal("rules file with no usable patterns must error")
	}
}

func TestRegexRulesLineNumberInError(t *testing.T) {
	path := writeRules(t, "^(?P<url>\\S+):(?P<login>[^:]+):(?P<password>.+)$\n^(bad$\n")
	_, _, err := NewRegexRulesParser(path)
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("error should name line 2, got %v", err)
	}
}

func TestRegexRulesHygieneViaFinishParse(t *testing.T) {
	// Even when a rule matches, finishParse hygiene still applies (>64 pw).
	path := writeRules(t, `^(?P<url>\S+);(?P<login>[^;]+);(?P<password>.+)$`)
	p, _, err := NewRegexRulesParser(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, ok := p.Parse("example.com;u;" + strings.Repeat("x", 65)); ok {
		t.Fatal("over-long password accepted")
	}
}

func TestDedupKeyWithRules(t *testing.T) {
	path := writeRules(t, `^(?P<url>\S+)\|(?P<login>[^|]+)\|(?P<password>.+)$`)
	p, _, err := NewRegexRulesParser(path)
	if err != nil {
		t.Fatal(err)
	}
	ka, okA := DedupKeyWith(p, "example.com|user|pw")
	kb, okB := DedupKeyForLine("example.com:user:pw", false)
	if !okA || !okB || ka != kb {
		t.Fatalf("key mismatch: %#x/%v vs %#x/%v", ka, okA, kb, okB)
	}
}
