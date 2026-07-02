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

// A loose (non-archived) input file is scanned for secrets alongside the ULP
// parse, through a full Engine.Run. Discovery only enqueues credential-looking
// loose files, so embed a token in a Passwords.txt to exercise the path.
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
