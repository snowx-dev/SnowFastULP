package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeZipWithDuplicateMember writes a zip that repeats one member name
// twice; ZIP permits duplicate member names.
func writeZipWithDuplicateMember(t *testing.T, path string, entries [][2]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range entries {
		w, err := zw.Create(e[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTdataDuplicateMemberConflictFlagsSource covers review H-19: two archive
// members with the SAME name but different bytes used to hit the
// destOwners[relDest] == memberName branch, which removed the staged file and
// rewrote it — silently replacing session data while the source stayed clean
// and deletion-eligible. The conflict must now be treated like a staging
// collision: prefixes dropped, nothing promoted, source flagged HadIssue.
func TestTdataDuplicateMemberConflictFlagsSource(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "log.zip")
	writeZipWithDuplicateMember(t, archivePath, [][2]string{
		{"VictimA/tdata/key_datas", "FIRST-KEY"},
		{"VictimA/tdata/key_datas", "SECOND-KEY"},
		{"VictimA/tdata/D877F783D5D3EF8C/maps0", "maps"},
		// A valid credential so the archive is not classified NoULP.
		{"Passwords.txt", "URL: https://a.example.com/\nUSER: u\nPASS: p\n"},
	})

	root := filepath.Join(t.TempDir(), "env-dest")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out bytes.Buffer
	_, results, err := e.Run(context.Background(), archivePath, &out)
	if err != nil {
		t.Fatal(err)
	}
	es := copier.Close()

	if es.DirsCopied != 0 {
		t.Fatalf("DirsCopied = %d, want 0 (conflicting duplicate must not promote)", es.DirsCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_datas")); !os.IsNotExist(err) {
		t.Fatalf("conflicting tdata was promoted: %v", err)
	}
	found := false
	for _, r := range results {
		if filepath.Base(r.Path) != "log.zip" {
			continue
		}
		found = true
		if !r.HadIssue {
			t.Fatalf("result = %+v, want HadIssue=true so -del keeps the archive", r)
		}
	}
	if !found {
		t.Fatalf("no result for log.zip in %+v", results)
	}
}

// TestTdataDuplicateMemberIdenticalKept is the positive control: a duplicate
// member that is byte-identical to the first entry stays a clean duplicate —
// the tdata promotes with the first entry's bytes and no issue.
func TestTdataDuplicateMemberIdenticalKept(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "log.zip")
	writeZipWithDuplicateMember(t, archivePath, [][2]string{
		{"VictimA/tdata/key_datas", "SAME-KEY"},
		{"VictimA/tdata/key_datas", "SAME-KEY"},
		{"VictimA/tdata/D877F783D5D3EF8C/maps0", "maps"},
		// A valid credential so the archive is not classified NoULP.
		{"Passwords.txt", "URL: https://a.example.com/\nUSER: u\nPASS: p\n"},
	})

	root := filepath.Join(t.TempDir(), "env-dest")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out bytes.Buffer
	_, results, err := e.Run(context.Background(), archivePath, &out)
	if err != nil {
		t.Fatal(err)
	}
	es := copier.Close()

	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (byte-identical duplicate is clean)", es.DirsCopied)
	}
	body, err := os.ReadFile(filepath.Join(root, "tdata", "key_datas"))
	if err != nil {
		t.Fatalf("promoted key_datas missing: %v", err)
	}
	if string(body) != "SAME-KEY" {
		t.Fatalf("promoted key_datas = %q, want the staged bytes", body)
	}
	for _, r := range results {
		if filepath.Base(r.Path) == "log.zip" && r.HadIssue {
			t.Fatalf("byte-identical duplicate flagged as issue: %+v", r)
		}
	}
}
