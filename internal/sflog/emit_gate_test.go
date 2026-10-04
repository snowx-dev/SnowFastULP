package sflog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineLooseUsesSharedParserForLabelLessInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Passwords.txt")
	if err := os.WriteFile(path, []byte("5.6.7.8:bob2:pw2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var strict bytes.Buffer
	strictStats, _, err := (&Engine{Workers: 1}).Run(context.Background(), path, &strict)
	if err != nil {
		t.Fatal(err)
	}
	if strictStats.Emitted != 0 {
		t.Fatalf("strict emitted %d lines, want 0 for bare-IP loose-only input", strictStats.Emitted)
	}

	var loose bytes.Buffer
	looseStats, results, err := (&Engine{Workers: 1, Loose: true}).Run(context.Background(), path, &loose)
	if err != nil {
		t.Fatal(err)
	}
	if looseStats.Emitted != 1 || loose.String() != "5.6.7.8:bob2:pw2\n" {
		t.Fatalf("loose emitted=%d output=%q, want one shared-parser record", looseStats.Emitted, loose.String())
	}
	if len(results) != 1 || results[0].HadIssue || !results[0].HistoryComplete {
		t.Fatalf("loose source result = %+v, want clean/history-complete", results)
	}
}

func TestEngineEmitGateNormalizesAndRejectsSharedParserFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Passwords.txt")
	body := "URL:  https://example.com/path  \nUSER:  Tinni Roy  \nPASS:  secret  \n\n" +
		"URL: https://example.net\nUSER: bad$user\nPASS: pw\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	stats, results, err := (&Engine{Workers: 1}).Run(context.Background(), path, &out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "example.com/path:Tinni Roy:secret\n"; got != want {
		t.Fatalf("output = %q, want normalized %q", got, want)
	}
	if stats.Emitted != 1 || stats.ParseErrors != 1 {
		t.Fatalf("stats emitted=%d parseErrors=%d, want 1/1", stats.Emitted, stats.ParseErrors)
	}
	if len(results) != 1 || !results[0].OK || !results[0].HadIssue || !results[0].HistoryComplete {
		t.Fatalf("source result = %+v, want parsed with issue, history complete (quality no longer withholds history)", results)
	}
	found := false
	for _, issue := range stats.Issues {
		if issue.Kind == IssueParseError && strings.Contains(IssueDetail(issue), "shared parser rejected credential: malformed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues = %+v, want shared-parser rejection", stats.Issues)
	}
}
