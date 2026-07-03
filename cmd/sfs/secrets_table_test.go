package main

import (
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestRenderSecretsTable(t *testing.T) {
	matches := []secrets.Match{
		{
			RuleName:   "JSON Web Token (base64url-encoded)",
			Secret:     "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig",
			Severity:   "high",
			SourcePath: "/home/x/STARLINK.zip!STARLINK/v4/[TR]84.17.86.91/Vivaldi/Default/History.txt",
			LastSeen:   time.Date(2026, 7, 3, 4, 29, 0, 0, time.UTC),
		},
		{
			RuleName:   "AWS S3 Bucket",
			Secret:     "/bassbuzz.s3.amazonaws.com",
			Severity:   "",
			SourcePath: "/home/x/STARLINK.zip!STARLINK/v4/[TR]84.17.86.91/Vivaldi/Default/History.txt",
			LastSeen:   time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
		},
	}
	got := renderSecretsTable(matches, 120)
	for _, want := range []string{"Type", "Secret", "Severity", "Source", "Last seen", "JSON Web Token"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table missing %q:\n%s", want, got)
		}
	}
	// sourceTail should drop the archive prefix, keeping the inner path start
	// (the tail may truncate at narrow widths — that's the column cap doing its job).
	if strings.Contains(got, "/home/x/STARLINK.zip!") {
		t.Fatalf("table leaked archive prefix:\n%s", got)
	}
	if !strings.Contains(got, "STARLINK/v4") {
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

func TestSecretsProfile(t *testing.T) {
	if got := secretsProfile(false); got != termenv.Ascii {
		t.Errorf("secretsProfile(false) = %v, want Ascii", got)
	}
	// A TTY stdout must not be forced to Ascii — that's the bug this guards
	// (applyStderrColorProfile downgrades globally on a piped stderr). Skip
	// when stdout isn't a real TTY so the assertion stays honest in CI.
	if stdoutIsTTY() {
		if got := secretsProfile(true); got == termenv.Ascii {
			t.Errorf("secretsProfile(true) = Ascii on a TTY stdout; want a color profile")
		}
	}
}

// TestRenderSecretsTableRespectsColorProfile is a render-level regression for
// the profile save/restore: under Ascii the table emits no ANSI escapes, under
// a color profile it does. Deterministic regardless of the test runner's TTY
// status because the profile is set explicitly.
func TestRenderSecretsTableRespectsColorProfile(t *testing.T) {
	matches := []secrets.Match{
		{RuleName: "JSON Web Token", Secret: "x.y.z", Severity: "high",
			SourcePath: "/a/b.zip!inner.txt", LastSeen: time.Date(2026, 7, 3, 4, 29, 0, 0, time.UTC)},
	}

	r := lipgloss.DefaultRenderer()
	prev := r.ColorProfile()
	defer r.SetColorProfile(prev)

	r.SetColorProfile(termenv.Ascii)
	if got := renderSecretsTable(matches, 120); strings.Contains(got, "\x1b[") {
		t.Errorf("Ascii profile leaked ANSI escapes:\n%s", got)
	}

	r.SetColorProfile(termenv.TrueColor)
	if got := renderSecretsTable(matches, 120); !strings.Contains(got, "\x1b[") {
		t.Errorf("TrueColor profile produced no ANSI escapes:\n%s", got)
	}
}
