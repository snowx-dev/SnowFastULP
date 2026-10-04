package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H-01-class escape (re-review): a -json target that is a symlink to the
// not-yet-created history database dangles at validation time — canonical
// compare and SameFile both pass — and the stream's create follows the link,
// materializing and truncating the database the moment history opens. The
// collision check must resolve the target's symlink chain and refuse.
func TestJSONOutSymlinkToHistoryDatabaseRefused(t *testing.T) {
	dir := t.TempDir()
	input := sfuSpaceFormInput(t, dir)
	histDir := filepath.Join(dir, "hist")
	if err := os.MkdirAll(histDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(histDir, "history.sqlite3")
	link := filepath.Join(histDir, "link.jsonl")
	if err := os.Symlink(db, link); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.d")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-history", "-history-path", histDir,
		"-json", link,
		"-o", out+string(filepath.Separator), input)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "overlaps history database") {
		t.Fatalf("stderr missing the collision reason:\n%s", stderr)
	}
	if _, err := os.Stat(db); err == nil {
		data, rerr := os.ReadFile(db)
		if rerr == nil && len(data) > 0 && !isSQLitePrefix(data) {
			t.Fatalf("dangling-symlink run materialized NDJSON in the database path:\n%q", data)
		}
	}
}

// The same escape against an input: a dangling symlink whose referent lands
// inside the scanned input directory would be created there by the stream.
func TestJSONOutSymlinkIntoInputRefused(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "data.txt")
	if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(filepath.Join(root, "new.txt"), link); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.d")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-json", link,
		"-o", out+string(filepath.Separator), root)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "overlaps input") {
		t.Fatalf("stderr missing the collision reason:\n%s", stderr)
	}
}

// A -json target naming the -parse-rules file would truncate it when the
// stream opens: the rules are an input and must be in the collision set.
func TestJSONOutTargetCollidingWithParseRulesRefused(t *testing.T) {
	dir := t.TempDir()
	input := sfuSpaceFormInput(t, dir)
	rules := filepath.Join(dir, "rules.conf")
	if err := os.WriteFile(rules, []byte(`^(?P<host>[^:]+):(?P<login>[^:]+):(?P<password>.+)$`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.d")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-parse-rules", rules,
		"-json", rules,
		"-o", out+string(filepath.Separator), input)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "overlaps parse rules") {
		t.Fatalf("stderr missing the collision reason:\n%s", stderr)
	}
	data, err := os.ReadFile(rules)
	if err != nil || !strings.HasPrefix(string(data), "^(?P<host>") {
		t.Fatalf("parse rules file destroyed: %q (err %v)", data, err)
	}
}

// isSQLitePrefix reports whether data starts with the SQLite file header.
func isSQLitePrefix(data []byte) bool {
	const magic = "SQLite format 3\x00"
	return len(data) >= len(magic) && string(data[:len(magic)]) == magic
}
