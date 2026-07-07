package sflog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const awsKeyLine = "aws_access_key_id = AKIAIOSFODNN7EXAMPLE"

// A loose credential file (Passwords.txt) is scanned for secrets alongside the
// ULP parse, through a full Engine.Run (the kindFile path in processFile).
func TestLooseFileScannedForSecrets(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "Passwords.txt",
		"URL: https://x\nUSER: a\nPASS: b\ntoken: ghp_1234567890abcdefghijklmnopqrstuvwx12\n")
	c := &capSink{}
	e := &Engine{Workers: 1, SecretSink: c, SecretMaxLen: defaultSecretMaxLen}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !c.sawSecret("Passwords.txt", "ghp_") {
		t.Fatalf("loose file was not scanned for secrets; got %v", c.got)
	}
}

// Phase B widening: with a sink wired, discovery enqueues arbitrary loose files
// (not just credential dumps) as kindSecretScan and scans them — a plain
// config.env that no ULP walker would ever pick up still yields its secret.
func TestLooseNonCredFileScannedForSecrets(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "config.env", awsKeyLine+"\n")
	c := &capSink{}
	e := &Engine{Workers: 1, SecretSink: c, SecretMaxLen: defaultSecretMaxLen}
	var out strings.Builder
	stats, _, err := e.Run(context.Background(), dir, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !c.sawSecret("config.env", "AKIA") {
		t.Fatalf("non-credential loose file was not scanned; got %v", c.got)
	}
	if stats.SecretFiles != 1 {
		t.Fatalf("SecretFiles = %d, want 1", stats.SecretFiles)
	}
	// A secret-scan file must not be miscounted as a credential file or a
	// no-ULP issue.
	if stats.FilesScanned != 0 || stats.NoULP != 0 {
		t.Fatalf("credential accounting leaked: FilesScanned=%d NoULP=%d", stats.FilesScanned, stats.NoULP)
	}
}

// A loose file that is not on the secret allowlist (e.g. a .png) is skipped
// entirely even with a sink wired: not scanned, not enqueued, not counted.
func TestLooseNonAllowlistedFileSkipped(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "photo.png", awsKeyLine+"\n")
	c := &capSink{}
	e := &Engine{Workers: 1, SecretSink: c, SecretMaxLen: defaultSecretMaxLen}
	var out strings.Builder
	stats, _, err := e.Run(context.Background(), dir, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(c.got) != 0 {
		t.Fatalf("non-allowlisted file must not be scanned; got %v", c.got)
	}
	if stats.SecretFiles != 0 {
		t.Fatalf("SecretFiles = %d, want 0", stats.SecretFiles)
	}
}

func TestIsSecretScanCandidate(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"a/b/config.env", true},
		{".env", true},
		{".env.local", true},
		{".env.production", true},
		{"notes.txt", true},
		{"report.PDF", true}, // case-insensitive
		{"settings.json", true},
		{"id_rsa", true},
		{".npmrc", true},
		{"CREDENTIALS", true},
		{"deploy.sh", true},
		{"photo.png", false},
		{"clip.mp4", false},
		{"archive.bin", false},
		{"noext", false},
	}
	for _, tc := range cases {
		if got := isSecretScanCandidate(tc.path); got != tc.want {
			t.Errorf("isSecretScanCandidate(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// Without a sink, discovery must stay credential-only: a plain config.env is
// neither scanned nor enqueued (byte-for-byte the pre-secrets behaviour).
func TestLooseNonCredFileIgnoredWithoutSink(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "config.env", awsKeyLine+"\n")
	e := &Engine{Workers: 1}
	var out strings.Builder
	stats, _, err := e.Run(context.Background(), dir, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.SecretFiles != 0 || stats.FilesScanned != 0 {
		t.Fatalf("non-credential file should be skipped without a sink: SecretFiles=%d FilesScanned=%d",
			stats.SecretFiles, stats.FilesScanned)
	}
}

func TestRarRoutesNonCredMemberToSecrets(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "Passwords.txt", "URL: https://x\nUSER: a\nPASS: b\n")
	mustWrite(t, dir, "env.txt", awsKeyLine)
	cmd := exec.Command(rarBin, "a", "-m0", "-ep1", "-idq", "log.rar", "Passwords.txt", "env.txt")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	rarPath := filepath.Join(dir, "log.rar")

	c := &capSink{}
	if _, err := readArchiveCredentials(context.Background(), rarPath, secretEC(t, rarPath, c), 1<<20); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !c.sawSecret("env.txt", "AKIA") {
		t.Fatalf("rar secret sink never saw env.txt; got %v", c.got)
	}
}

func TestSevenZipRoutesNonCredMemberToSecrets(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "Passwords.txt", "URL: https://x\nUSER: a\nPASS: b\n")
	mustWrite(t, dir, "env.txt", awsKeyLine)
	cmd := exec.Command(bin, "a", "-y", "-bso0", "-bsp0", "log.7z", "Passwords.txt", "env.txt")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("7z create failed: %v\n%s", e, out)
	}
	path := filepath.Join(dir, "log.7z")

	c := &capSink{}
	if _, err := readArchiveCredentials(context.Background(), path, secretEC(t, path, c), 1<<20); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !c.sawSecret("env.txt", "AKIA") {
		t.Fatalf("7z secret sink never saw env.txt; got %v", c.got)
	}
}

func mustWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func secretEC(t *testing.T, display string, c *capSink) extractCtx {
	t.Helper()
	return extractCtx{
		passwords:    []string{""},
		tempDir:      t.TempDir(),
		display:      display,
		emit:         func(Credential) {},
		onIssue:      func(string, IssueKind, error) {},
		processor:    defaultProcessor,
		secrets:      c,
		secretMaxLen: defaultSecretMaxLen,
	}
}
