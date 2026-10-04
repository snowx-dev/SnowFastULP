package sflog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLooseTdataNameCollisionKeepsSource covers review H-18: on Unix, `ab`
// and `a\b` are distinct valid filenames but sanitizePathElem normalizes both
// to `ab`, so the loose-tree copy used to overwrite one with the other via
// O_TRUNC and still classify the source as clean (deletion-eligible under
// -del). The copy must now fail as a collision: no flattened destination
// file, and the source is flagged so -del keeps it.
func TestLooseTdataNameCollisionKeepsSource(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "VictimA", "tdata")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "ab"), []byte("AAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "a\\b"), []byte("BBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	// key_datas marks the folder as a real Telegram tdata tree.
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env-dest")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	e := &Engine{Workers: 1, EnvCopier: copier}
	var out bytes.Buffer
	_, results, err := e.Run(context.Background(), dir, &out)
	if err != nil {
		t.Fatal(err)
	}
	es := copier.Close()

	// No destination tree may exist: the colliding pair must never be
	// flattened into a single overwritten file.
	if b, err := os.ReadFile(filepath.Join(root, "tdata", "ab")); err == nil {
		t.Fatalf("collision silently flattened into one destination file (content %q)", b)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata")); !os.IsNotExist(err) {
		t.Fatalf("partial destination tree survived a collision-failed copy: %v", err)
	}
	if es.TdataErrors != 1 {
		t.Fatalf("TdataErrors = %d, want 1", es.TdataErrors)
	}
	found := false
	for _, r := range results {
		if !strings.Contains(r.Path, "VictimA") {
			continue
		}
		found = true
		if r.OK || !r.HadIssue {
			t.Fatalf("result = %+v, want OK=false HadIssue=true so -del keeps the source", r)
		}
	}
	if !found {
		t.Fatalf("no result for VictimA in %+v", results)
	}
}

// TestLooseTdataWithoutCollisionStillCopies is the negative control: a tree
// whose names only look risky (backslash-free) still copies cleanly.
func TestLooseTdataWithoutCollisionStillCopies(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "VictimA", "tdata")
	if err := os.MkdirAll(filepath.Join(td, "D877F783D5D3EF8C"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "D877F783D5D3EF8C", "maps0"), []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env-dest")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	e := &Engine{Workers: 1, EnvCopier: copier}
	var out bytes.Buffer
	_, results, err := e.Run(context.Background(), dir, &out)
	if err != nil {
		t.Fatal(err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 || es.TdataErrors != 0 {
		t.Fatalf("stats = DirsCopied %d TdataErrors %d, want 1/0", es.DirsCopied, es.TdataErrors)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_datas")); err != nil {
		t.Fatalf("key_datas not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "D877F783D5D3EF8C", "maps0")); err != nil {
		t.Fatalf("nested file not copied: %v", err)
	}
	for _, r := range results {
		if strings.Contains(r.Path, "VictimA") && (r.HadIssue) {
			t.Fatalf("clean copy flagged as issue: %+v", r)
		}
	}
}
