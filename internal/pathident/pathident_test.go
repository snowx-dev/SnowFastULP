package pathident_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/pathident"
)

func TestSameFileDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	same, err := pathident.SameFile(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Fatal("distinct files reported as same")
	}
}

func TestSameFileSelf(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	same, err := pathident.SameFile(a, a)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("a vs a not reported as same")
	}
}

func TestSameFileRelativeAbsAlias(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	same, err := pathident.SameFile("./a", a)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("./a vs absolute a not reported as same")
	}
}

func TestSameFileHardlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink test on non-Windows")
	}
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	link := filepath.Join(dir, "alink")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, link); err != nil {
		t.Skip("link not supported")
	}
	same, err := pathident.SameFile(a, link)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("hardlink not reported as same")
	}
}

func TestSameFileMissingPathIsFalse(t *testing.T) {
	dir := t.TempDir()
	same, err := pathident.SameFile(filepath.Join(dir, "nope"), filepath.Join(dir, "nada"))
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Fatal("missing files should not be 'same'")
	}
}
