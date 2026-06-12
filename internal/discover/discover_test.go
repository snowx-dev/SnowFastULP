package discover_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestListZstSinceFiltersByMtime(t *testing.T) {
	dir := t.TempDir()
	recent := filepath.Join(dir, "recent.zst")
	old := filepath.Join(dir, "old.zst")
	mustTouch(t, recent)
	mustTouch(t, old)

	// backdate old.zst to 30 days ago.
	old30 := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, old30, old30); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	paths, err := discover.ListZstSince(dir, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "recent.zst" {
		t.Fatalf("got %v, want only recent.zst", paths)
	}
}

func TestListZstSinceAllTooOld(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.zst")
	mustTouch(t, old)
	old30 := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, old30, old30); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	if _, err := discover.ListZstSince(dir, cutoff); err == nil {
		t.Fatal("expected error when all files older than cutoff")
	}
}

func mustTouch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
