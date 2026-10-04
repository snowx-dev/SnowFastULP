package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The false "no credential-named files found" regression: an archive whose
// credential member was found but failed to parse (here: one line over the
// per-line cap) must report the parse error and nothing else. Claiming no
// credential-named files were found for the same archive is factually false.
func TestEngineNoNoULPAfterMemberParseFailure(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "victim.zip")
	writePlainZip(t, archivePath, "Passwords.txt", strings.Repeat("x", 5<<20)+"\n")

	eng := &Engine{Workers: 1, Passwords: []string{""}}
	var out bytes.Buffer
	stats, results, err := eng.Run(context.Background(), archivePath, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].OK || !results[0].HadIssue {
		t.Fatalf("results = %+v, want one OK-but-HadIssue archive (member failure isolated)", results)
	}
	var sawParseError bool
	for _, is := range stats.Issues {
		if is.Kind == IssueNoULP {
			t.Fatalf("false no-ulp issue for the failed archive: %+v", stats.Issues)
		}
		if is.Kind == IssueParseError {
			sawParseError = true
		}
	}
	if !sawParseError {
		t.Fatalf("missing parse-error issue: %+v", stats.Issues)
	}
	if stats.NoULP != 0 {
		t.Fatalf("NoULP = %d, want 0 (a credential member was found but failed)", stats.NoULP)
	}
}

// The true no-ULP case must keep firing: an archive with no credential-named
// member (or one with no parseable credentials and no issues) still reports
// "no credential-named files found" — and nothing else.
func TestEngineNoULPStillFiresForEmptyArchive(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "empty.zip")
	writePlainZip(t, archivePath, "info.txt", "nothing in here\n")

	eng := &Engine{Workers: 1, Passwords: []string{""}}
	var out bytes.Buffer
	stats, _, err := eng.Run(context.Background(), archivePath, &out)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NoULP != 1 {
		t.Fatalf("NoULP = %d, want 1 for a credential-free archive", stats.NoULP)
	}
	sawNoULP := false
	for _, is := range stats.Issues {
		if is.Kind == IssueNoULP {
			sawNoULP = true
		}
	}
	if !sawNoULP {
		t.Fatalf("missing no-ulp issue: %+v", stats.Issues)
	}
}

func writePlainZip(t *testing.T, path, name, body string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
