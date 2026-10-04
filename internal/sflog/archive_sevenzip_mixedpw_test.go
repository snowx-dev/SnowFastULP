package sflog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A 7z whose members live in folders encrypted under DIFFERENT passwords
// cannot be decoded completely by any single candidate: each candidate decodes
// its own folder and fails the other one. An early member still streams its
// credentials (the gate is confirmed mid-attempt), so the final
// password-not-found failure must retain those partial scan statistics and
// name the member that kept rejecting every candidate — not report an empty
// scan against one emitted credential.
func TestMixedPasswordSevenZipKeepsPartialStatsAndNamesMember(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	for _, sub := range []string{"A", "B"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(dir, "B"), "Passwords.txt", "URL: https://two.example.com\nUSER: u2\nPASS: p2\n")
	mustWrite(t, filepath.Join(dir, "A"), "Passwords.txt", "URL: https://one.example.com\nUSER: u1\nPASS: p1\n")
	// B.txt is packed into a folder encrypted with "secret"; the update then
	// adds A.txt as a NEW folder encrypted with "alpha" (7z reuses the
	// existing packed stream), producing one archive with two folders under
	// two passwords — the mixed-password case no single candidate decodes.
	cmd := exec.Command(bin, "a", "-bso0", "-t7z", "-mx=0", "-psecret", "enc.7z", "B/Passwords.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7z create failed: %v\n%s", err, out)
	}
	cmd = exec.Command(bin, "u", "-bso0", "-t7z", "-mx=9", "-palpha", "enc.7z", "A/Passwords.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("7z update failed: %v\n%s", err, out)
	}

	path := filepath.Join(dir, "enc.7z")
	var got []Credential
	ec := extractCtx{
		passwords: []string{"", "alpha", "secret"},
		display:   path,
		emit:      func(c Credential) { got = append(got, c) },
		onIssue:   func(string, IssueKind, error) {},
	}
	scan, err := readArchiveCredentials(context.Background(), path, ec, 1<<20)
	t.Logf("DEBUG scan=%+v err=%v creds=%+v", scan, err, got)
	if !errors.Is(err, errPasswordNotFound) {
		t.Fatalf("error = %v, want password-not-found (no single candidate decodes both folders)", err)
	}
	if !strings.Contains(err.Error(), `"A/Passwords.txt"`) {
		t.Fatalf("error = %v, want the still-rejected member named", err)
	}
	if scan.files != 1 {
		t.Fatalf("scan.files = %d, want the partially scanned credential member retained", scan.files)
	}
	if len(got) != 1 || got[0].Password != "p2" {
		t.Fatalf("credentials = %+v, want the one member that decoded (B/Passwords.txt under secret)", got)
	}
}
