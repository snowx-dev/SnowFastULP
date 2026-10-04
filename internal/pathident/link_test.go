package pathident_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/pathident"
)

// The H-01-class dangling-symlink escape: a -json target whose final
// component is a (possibly dangling) symlink passes canonical/SameFile
// preflights because the referent does not exist yet, and the stream's
// create follows the link onto the protected path. LinkResolution must
// surface the referent chain with the dangling case resolved as far as it
// goes, so preflights can compare it.
func TestLinkResolutionDanglingReferent(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(filepath.Join(sub, "history.sqlite3"), link); err != nil {
		t.Fatal(err)
	}
	refs, err := pathident.LinkResolution(link)
	if err != nil {
		t.Fatalf("LinkResolution: %v", err)
	}
	if len(refs) != 1 || refs[0] != filepath.Join(sub, "history.sqlite3") {
		t.Fatalf("refs = %v, want [%s]", refs, filepath.Join(sub, "history.sqlite3"))
	}
}

func TestLinkResolutionChain(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "b")
	if err := os.Symlink(target, b); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	refs, err := pathident.LinkResolution(a)
	if err != nil {
		t.Fatalf("LinkResolution: %v", err)
	}
	// Canonicalization collapses the existing intermediate hop; the final
	// referent must be the chain's target.
	if len(refs) == 0 || refs[len(refs)-1] != target {
		t.Fatalf("refs = %v, want final hop %s", refs, target)
	}
}

func TestLinkResolutionNotALink(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := pathident.LinkResolution(plain)
	if err != nil {
		t.Fatalf("LinkResolution: %v", err)
	}
	if refs != nil {
		t.Fatalf("plain file: refs = %v, want nil", refs)
	}
}

func TestLinkRefersTo(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "history.sqlite3")
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	// Dangling referent: still refers to the protected path.
	if !pathident.LinkRefersTo(link, db) {
		t.Fatal("dangling symlink to the DB not recognized")
	}
	// The link also refers to itself as the DB spelling through the chain.
	if pathident.LinkRefersTo(link, filepath.Join(dir, "other.jsonl")) {
		t.Fatal("unrelated path reported as referent")
	}
	// A plain path is never a "link referent".
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pathident.LinkRefersTo(db, db) {
		t.Fatal("non-symlink path reported as link referent")
	}
	// Once the DB exists, the link still refers to it.
	if !pathident.LinkRefersTo(link, db) {
		t.Fatal("symlink to existing DB not recognized")
	}
}

func TestLinkRefersWithinDir(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// Dangling referent landing inside the directory.
	into := filepath.Join(dir, "into")
	if err := os.Symlink(filepath.Join(lib, "new.jsonl"), into); err != nil {
		t.Fatal(err)
	}
	if !pathident.LinkRefersWithinDir(into, lib) {
		t.Fatal("dangling referent inside the directory not recognized")
	}
	// Referent equal to the directory itself.
	onto := filepath.Join(dir, "onto")
	if err := os.Symlink(lib, onto); err != nil {
		t.Fatal(err)
	}
	if !pathident.LinkRefersWithinDir(onto, lib) {
		t.Fatal("referent equal to the directory not recognized")
	}
	// Existing referent inside the directory.
	if err := os.WriteFile(filepath.Join(lib, "hits.jsonl"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "live")
	if err := os.Symlink(filepath.Join(lib, "hits.jsonl"), live); err != nil {
		t.Fatal(err)
	}
	if !pathident.LinkRefersWithinDir(live, lib) {
		t.Fatal("existing referent inside the directory not recognized")
	}
	// Referents outside the directory never count, and a non-symlink path
	// is never a link referent.
	outside := filepath.Join(dir, "outside")
	if err := os.Symlink(filepath.Join(dir, "stats.jsonl"), outside); err != nil {
		t.Fatal(err)
	}
	if pathident.LinkRefersWithinDir(outside, lib) {
		t.Fatal("unrelated referent reported inside the directory")
	}
	if pathident.LinkRefersWithinDir(filepath.Join(lib, "hits.jsonl"), lib) {
		t.Fatal("plain path reported as a link referent")
	}
}
