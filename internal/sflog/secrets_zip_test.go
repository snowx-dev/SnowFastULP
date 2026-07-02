package sflog

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A zip containing a non-password file with an AWS key must reach the secret
// sink, tagged with the member provenance, while the credential member is still
// parsed normally.
func TestZipRoutesNonCredMemberToSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.zip")
	writeTestZip(t, path, map[string]string{
		"Passwords.txt": "URL: https://x\nUSER: a\nPASS: b\n",
		"env.txt":       "aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
	})
	c := &capSink{}
	var creds int
	ec := extractCtx{
		passwords:    []string{""},
		tempDir:      t.TempDir(),
		emit:         func(Credential) { creds++ },
		onIssue:      func(string, IssueKind, error) {},
		processor:    defaultProcessor,
		secrets:      c,
		secretMaxLen: defaultSecretMaxLen,
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
		t.Fatalf("read: %v", err)
	}
	if creds == 0 {
		t.Fatal("credential member was not parsed")
	}
	found := false
	for _, g := range c.got {
		if strings.Contains(g, "env.txt") && strings.Contains(g, "AKIA") {
			found = true
		}
	}
	if !found {
		t.Fatalf("secret sink never saw env.txt; got %v", c.got)
	}
}

// With no sink wired the non-credential member is skipped exactly as before.
func TestZipNoSinkSkipsOtherMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.zip")
	writeTestZip(t, path, map[string]string{
		"Passwords.txt": "URL: https://x\nUSER: a\nPASS: b\n",
		"env.txt":       "aws_access_key_id = AKIAIOSFODNN7EXAMPLE",
	})
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		processor: defaultProcessor,
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
		t.Fatalf("read: %v", err)
	}
}
