package sflog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	zipenc "github.com/yeka/zip"
)

// ZIP permits per-entry passwords. One encrypted probe used to select a single
// password applied to every encrypted member, so a zip whose members are
// encrypted under two different supplied candidates lost the second member to
// a checksum parse error. Each encrypted member must get its own
// probe/retry over the candidates. Both WinZip AES (whose Open validates the
// encryption header eagerly) and legacy ZipCrypto (whose check byte fails at
// read/parse time) take the same per-member resolution.
func TestZipPerMemberPasswordsExtractAllMembers(t *testing.T) {
	t.Run("aes256", func(t *testing.T) {
		testZipPerMemberPasswords(t, zipenc.AES256Encryption)
	})
	t.Run("zipcrypto", func(t *testing.T) {
		testZipPerMemberPasswords(t, zipenc.StandardEncryption)
	})
}

func testZipPerMemberPasswords(t *testing.T, method zipenc.EncryptionMethod) {
	path := filepath.Join(t.TempDir(), "mixed.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zipenc.NewWriter(f)
	for _, m := range []struct{ name, password, body string }{
		{"alpha/Passwords.txt", "alpha", "URL: https://alpha.example.com\nUSER: ua\nPASS: pa\n"},
		{"beta/Passwords.txt", "beta", "URL: https://beta.example.com\nUSER: ub\nPASS: pb\n"},
	} {
		fh := &zipenc.FileHeader{Name: m.name, Method: zipenc.Deflate}
		fh.ModifiedTime, fh.ModifiedDate = msDosTime(time.Now()), 0x5921
		fh.SetPassword(m.password)
		fh.SetEncryptionMethod(method)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var got []Credential
	var issues []pendingIssue
	ec := extractCtx{
		passwords: []string{"alpha", "beta"},
		display:   path,
		emit:      func(c Credential) { got = append(got, c) },
		onIssue:   func(p string, k IssueKind, e error) { issues = append(issues, pendingIssue{p, k, e}) },
	}
	scan, err := readArchiveCredentials(context.Background(), path, ec, 1<<20)
	if err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if len(issues) != 0 {
		msgs := make([]string, 0, len(issues))
		for _, is := range issues {
			msgs = append(msgs, fmt.Sprintf("%s: %v", is.path, is.err))
		}
		t.Fatalf("issues = %q, want none (both members' passwords were supplied)", msgs)
	}
	if len(got) != 2 || scan.files != 2 {
		t.Fatalf("got %d credential(s), scan.files = %d; want both members extracted", len(got), scan.files)
	}
	if got[0].Password != "pa" || got[1].Password != "pb" {
		t.Fatalf("credentials = %+v, want one per member password", got)
	}
}
