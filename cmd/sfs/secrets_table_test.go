package main

import (
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"
)

func TestRenderSecretsTable(t *testing.T) {
	matches := []secrets.Match{
		{
			RuleName:   "JSON Web Token (base64url-encoded)",
			Secret:     "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig",
			Severity:   "high",
			SourcePath: "/home/x/sample.zip!sample/v4/[TR]203.0.113.91/Vivaldi/Default/History.txt",
			LastSeen:   time.Date(2026, 7, 3, 4, 29, 0, 0, time.UTC),
		},
		{
			RuleName:   "AWS S3 Bucket",
			Secret:     "/bassbuzz.s3.amazonaws.com",
			Severity:   "",
			SourcePath: "/home/x/sample.zip!sample/v4/[TR]203.0.113.91/Vivaldi/Default/History.txt",
			LastSeen:   time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
		},
	}
	got := renderSecretsTable(matches, 120)
	for _, want := range []string{"Type", "Secret", "Source", "Last seen", "JSON Web Token"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Severity") {
		t.Fatalf("Severity column should be removed:\n%s", got)
	}
	// sourceTail should drop the archive prefix, keeping the inner path start
	// (the tail may truncate at narrow widths — that's the column cap doing its job).
	if strings.Contains(got, "/home/x/sample.zip!") {
		t.Fatalf("table leaked archive prefix:\n%s", got)
	}
	if !strings.Contains(got, "sample/v4") {
		t.Fatalf("table lost inner source path:\n%s", got)
	}
	// rounded border present
	if !strings.Contains(got, "╭") || !strings.Contains(got, "╯") {
		t.Fatalf("table missing rounded borders:\n%s", got)
	}
}

func TestSourceTail(t *testing.T) {
	cases := map[string]string{
		"/a/b.zip!inner/path.txt":      "inner/path.txt",
		"/a/b.txt":                     "/a/b.txt",
		"":                             "",
		"archive.zip!":                 "archive.zip!", // trailing ! with no inner → keep as-is
		"no-bang-here/just/a/path.csv": "no-bang-here/just/a/path.csv",
	}
	for in, want := range cases {
		if got := sourceTail(in); got != want {
			t.Errorf("sourceTail(%q) = %q, want %q", in, got, want)
		}
	}
}
