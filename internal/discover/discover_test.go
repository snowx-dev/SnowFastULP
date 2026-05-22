package discover_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/discover"
)

func TestListZstSorted(t *testing.T) {
	dir := t.TempDir()
	mustTouch(t, filepath.Join(dir, "b.zst"))
	mustTouch(t, filepath.Join(dir, "a.zst"))
	mustTouch(t, filepath.Join(dir, "note.txt"))

	paths, err := discover.ListZst(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("got %d paths", len(paths))
	}
	if filepath.Base(paths[0]) != "a.zst" || filepath.Base(paths[1]) != "b.zst" {
		t.Fatalf("order = %v", paths)
	}
}

func TestListZstEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := discover.ListZst(dir)
	if err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func TestListTxtSorted(t *testing.T) {
	dir := t.TempDir()
	mustTouch(t, filepath.Join(dir, "b.txt"))
	mustTouch(t, filepath.Join(dir, "a.txt"))
	mustTouch(t, filepath.Join(dir, "skip.zst"))

	paths, err := discover.ListTxt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("got %d paths", len(paths))
	}
	if filepath.Base(paths[0]) != "a.txt" || filepath.Base(paths[1]) != "b.txt" {
		t.Fatalf("order = %v", paths)
	}
}

func TestListTxtExcludesTxtZst(t *testing.T) {
	dir := t.TempDir()
	mustTouch(t, filepath.Join(dir, "archive.txt.zst"))
	_, err := discover.ListTxt(dir)
	if err == nil {
		t.Fatal("expected error when only .txt.zst present")
	}
}

func TestListTxtEmpty(t *testing.T) {
	dir := t.TempDir()
	_, err := discover.ListTxt(dir)
	if err == nil {
		t.Fatal("expected error for empty dir")
	}
}

func mustTouch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
