package sflog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveSignatureOK(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name   string
		ext    string
		data   []byte
		wantOK bool
	}{
		{"real.7z", ".7z", []byte("\x37\x7A\xBC\xAF\x27\x1C\x00\x04rest"), true},
		{"decoy.7z", ".7z", []byte("this is not a 7z file"), false},
		{"tiny.7z", ".7z", []byte("\x37\x7A"), false}, // truncated below signature
		{"lfh.zip", ".zip", []byte("PK\x03\x04rest of a zip"), true},
		{"eocd.zip", ".zip", []byte("PK\x05\x06"), true},
		{"decoy.zip", ".zip", []byte("not a zip at all"), false},
		{"sfx.rar", ".rar", []byte("MZ\x90\x00 self-extracting stub"), true}, // .rar not vetted here
		{"note.txt", ".txt", []byte("just text"), true},                      // non-archive ext skipped
	}
	for _, tc := range cases {
		p := write(tc.name, tc.data)
		ok, err := archiveSignatureOK(p, tc.ext)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if ok != tc.wantOK {
			t.Errorf("%s: archiveSignatureOK = %v, want %v", tc.name, ok, tc.wantOK)
		}
	}
}

// A member named *.7z that isn't a 7z (a stealer-log decoy) must be skipped as
// a parse/skip issue without a password sweep, never reported as a missing
// password. The real password file alongside it must still be extracted.
func TestNestedDecoy7zSkippedAsParseNotPassword(t *testing.T) {
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{
		"victim/Downloads/decoy.7z": []byte("definitely not a 7-zip archive\n"),
		"victim/Passwords.txt":      []byte("URL: https://x.example/login\nUSER: a\nPASS: p\n"),
	})

	rec := &stageRecorder{}
	var issueKinds []IssueKind
	var emitted int
	ec := extractCtx{
		passwords: []string{"", "wrong", "ice"},
		tempDir:   t.TempDir(),
		display:   outer,
		emit:      func(Credential) { emitted++ },
		onIssue:   func(_ string, kind IssueKind, _ error) { issueKinds = append(issueKinds, kind) },
		setStage:  rec.sink(),
	}
	if _, err := readArchiveCredentials(context.Background(), outer, ec, 100); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if emitted != 1 {
		t.Fatalf("emitted = %d, want 1 (the real Passwords.txt)", emitted)
	}
	if len(issueKinds) != 1 || issueKinds[0] != IssueParseError {
		t.Fatalf("decoy issue kinds = %v, want exactly [IssueParseError]", issueKinds)
	}
	if rec.has(StageTestingPassword) {
		t.Fatalf("decoy must skip the password sweep entirely; stages: %v", rec.stages)
	}
}

// While inside a nested archive the worker line must name that archive, then
// restore the parent label when it returns, so the live row never reads as the
// parent doing the nested archive's stage.
func TestRecurseNestedPublishesThenRestoresWorkerItem(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://inner.example/login\nUSER: a\nPASS: p\n"),
	})
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{
		"nest/inner.zip": inner,
	})

	var items []string
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   outer,
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		setItem:   func(s string) { items = append(items, s) },
	}
	if _, err := readArchiveCredentials(context.Background(), outer, ec, 100); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}

	var sawNested bool
	for _, it := range items {
		if strings.Contains(it, "!") && strings.Contains(it, "inner.zip") {
			sawNested = true
		}
	}
	if !sawNested {
		t.Fatalf("never published the nested archive label: %v", items)
	}
	if len(items) == 0 || items[len(items)-1] != outer {
		t.Fatalf("last item label = %v, want the restored parent %q", items, outer)
	}
}
