package sflog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLooseHistoryComplete(t *testing.T) {
	for _, tc := range []struct {
		name, body          string
		complete, ok, issue bool
	}{
		{"credentials", "URL: a.com\nUSER: u\nPASS: p\n", true, true, false},
		{"no ulp", "Browser: Chrome\n", true, true, true},
		// preservation disabled 2026-09-30 (user): parse failures record.
		{"parse failure", string(bytes.Repeat([]byte("x"), 5<<20)), true, false, false},
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
			if len(results) != 1 {
				t.Fatalf("results=%+v", results)
			}
			r := results[0]
			if r.HistoryComplete != tc.complete || r.OK != tc.ok || r.HadIssue != tc.issue {
				t.Fatalf("result=%+v want complete=%v ok=%v issue=%v", r, tc.complete, tc.ok, tc.issue)
			}
		})
	}
}

func TestArchiveHistoryComplete(t *testing.T) {
	for _, tc := range []struct {
		name                string
		make                func(*testing.T, string)
		complete, ok, issue bool
	}{
		{"credentials", func(t *testing.T, p string) {
			writeTestZip(t, p, map[string]string{"Passwords.txt": "URL: a.com\nUSER: u\nPASS: p\n"})
		}, true, true, false},
		{"no ulp", func(t *testing.T, p string) {
			writeTestZip(t, p, map[string]string{"Passwords.txt": "Browser: Chrome\n"})
		}, true, true, true},
		// preservation disabled 2026-09-30 (user): locked archives record.
		{"wrong password", func(t *testing.T, p string) {
			writeEncryptedTestZip(t, p, "secret", "Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")
		}, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "logs.zip")
			tc.make(t, p)
			_, results, err := (&Engine{Workers: 1, Passwords: []string{"wrong"}}).Run(context.Background(), p, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 {
				t.Fatalf("results=%+v", results)
			}
			r := results[0]
			if r.HistoryComplete != tc.complete || r.OK != tc.ok || r.HadIssue != tc.issue {
				t.Fatalf("result=%+v want complete=%v ok=%v issue=%v", r, tc.complete, tc.ok, tc.issue)
			}
		})
	}
}
