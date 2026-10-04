package sflog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mixedBody is a realistic hand-combined stealer log: one labeled block plus
// one valid label-less url:user:password line (review H-20).
const mixedBody = "URL: https://a.example.com/\n" +
	"USER: alice\n" +
	"PASS: secret\n" +
	"https://b.example.com/signin:bob:hunter2\n"

// TestMixedFormatFlagsSourceKeepsOutput covers review H-20: labeled
// precedence must stay (output unchanged), but a source whose colon lines
// were discarded is flagged HadIssue for reporting; since 2026-09-30 it
// still records history complete and -del may delete it (parse quality no
// longer withholds either).
func TestMixedFormatFlagsSourceKeepsOutput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "Passwords.txt")
	if err := os.WriteFile(p, []byte(mixedBody), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stats, results, err := (&Engine{Workers: 1}).Run(context.Background(), p, &out)
	if err != nil {
		t.Fatal(err)
	}

	// Output unchanged: the labeled credential is emitted, the discarded
	// colon credential is not (product decision: labeled precedence kept).
	body := out.String()
	if !strings.Contains(body, "alice") {
		t.Fatalf("labeled credential missing from output: %q", body)
	}
	if strings.Contains(body, "bob") {
		t.Fatalf("output changed: colon credential must stay discarded: %q", body)
	}

	found := false
	for _, r := range results {
		if r.Path != p {
			continue
		}
		found = true
		if !r.OK {
			t.Fatalf("result = %+v, want OK=true (it parsed)", r)
		}
		if !r.HadIssue {
			t.Fatalf("result = %+v, want HadIssue=true so the run reports the discard", r)
		}
		if !r.HistoryComplete {
			t.Fatalf("result = %+v, want HistoryComplete=true (parse quality no longer withholds history)", r)
		}
	}
	if !found {
		t.Fatalf("no result for %s in %+v", p, results)
	}
	hasMixed := false
	for _, is := range stats.Issues {
		if is.Kind == IssueMixedFormat && is.Path == p {
			hasMixed = true
		}
	}
	if !hasMixed {
		t.Fatalf("issues = %+v, want a mixed-format issue for the source", stats.Issues)
	}
}

// TestMixedFormatInsideArchiveFlagsSource: the same rule for an archive
// member — a mixed member sets the archive's HadIssue (reporting the
// discard) while HistoryComplete stays true since 2026-09-30 (parse quality
// no longer withholds history or preserves the archive from -del).
func TestMixedFormatInsideArchiveFlagsSource(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "log.zip")
	writeTestZip(t, archivePath, map[string]string{
		"Passwords.txt": mixedBody,
	})
	var out bytes.Buffer
	_, results, err := (&Engine{Workers: 1}).Run(context.Background(), archivePath, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	r := results[0]
	if !r.IsArchive || !r.OK || !r.HadIssue {
		t.Fatalf("result = %+v, want IsArchive OK HadIssue", r)
	}
	if !r.HistoryComplete {
		t.Fatalf("result = %+v, want HistoryComplete=true (parse quality no longer withholds history)", r)
	}
}

// Negative control: a pure labeled file and a pure colon file stay clean.
func TestPureFormatSourcesStayClean(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"labeled only", "URL: https://a.example.com/\nUSER: alice\nPASS: secret\n"},
		{"colon only", "https://b.example.com/signin:bob:hunter2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "Passwords.txt")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, results, err := (&Engine{Workers: 1}).Run(context.Background(), p, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			r := results[0]
			if r.HadIssue || !r.HistoryComplete || !r.OK {
				t.Fatalf("pure-format source flagged: %+v", r)
			}
		})
	}
}
