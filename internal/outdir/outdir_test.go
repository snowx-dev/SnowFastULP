package outdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDirEmptyReturnsCWD(t *testing.T) {
	for _, flag := range []string{"-o", "-od", "-odr"} {
		dir, auto, err := ResolveDir(flag, "")
		if err != nil || dir != "." || !auto {
			t.Fatalf("%s empty: got dir=%q auto=%v err=%v", flag, dir, auto, err)
		}
	}
}

func TestResolveDirODAcceptsBareName(t *testing.T) {
	dir, auto, err := ResolveDir("-od", "library")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "library" || !auto {
		t.Fatalf("got dir=%q auto=%v", dir, auto)
	}
	dir, auto, err = ResolveDir("-odr", "preview-lib")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "preview-lib" || !auto {
		t.Fatalf("got dir=%q auto=%v", dir, auto)
	}
}

func TestResolveDirORejectsPlainFile(t *testing.T) {
	if _, _, err := ResolveDir("-o", "cleaned.txt"); err == nil {
		t.Fatal("expected -o cleaned.txt to be rejected")
	}
}

func TestResolveDirOAcceptsTrailingSep(t *testing.T) {
	want := "out" + string(os.PathSeparator)
	dir, auto, err := ResolveDir("-o", want)
	if err != nil {
		t.Fatal(err)
	}
	if !auto {
		t.Fatal("expected autoMkdir")
	}
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
}

func TestResolveDirOAcceptsExistingDir(t *testing.T) {
	dir := t.TempDir()
	got, auto, err := ResolveDir("-o", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !auto || got != dir {
		t.Fatalf("got dir=%q auto=%v, want %q auto=true", got, auto, dir)
	}
}

func TestEnsureReadyRejectsExistingFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		err := EnsureReady("-odr", file, create)
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("create=%v: want not-a-directory error, got %v", create, err)
		}
		if fi, serr := os.Stat(file); serr != nil || fi.IsDir() {
			t.Fatalf("create=%v: file should remain a file: fi=%v err=%v", create, fi, serr)
		}
	}
}

func TestEnsureReadyCreatesMissing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "newlib")
	if err := EnsureReady("-od", target, true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.IsDir() {
		t.Fatalf("expected created dir, got fi=%v err=%v", fi, err)
	}
}

func TestEnsureReadyDryRunMissingOK(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "preview-missing")
	if err := EnsureReady("-odr", target, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create dir, stat err=%v", err)
	}
}

func TestEnsureReadyExistingDirOK(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureReady("-od", dir, true); err != nil {
		t.Fatal(err)
	}
	if err := EnsureReady("-odr", dir, false); err != nil {
		t.Fatal(err)
	}
}

// P5-W8: whitespace in a quoted output dir is filesystem data, not noise.
// ResolveDir must preserve every byte of a nonempty path; only "" means the
// default. A dir named " leading", "trailing ", or " " must target exactly
// that name — never a silently trimmed sibling.
func TestResolveDirPreservesWhitespace(t *testing.T) {
	base := t.TempDir()
	// pre-create the whitespace dirs so -o's dir-hint check accepts them
	for _, name := range []string{" leading", "trailing ", " "} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, flag := range []string{"-o", "-od", "-odr"} {
		for _, name := range []string{" leading", "trailing ", " "} {
			in := filepath.Join(base, name)
			got, _, err := ResolveDir(flag, in)
			if err != nil {
				t.Fatalf("ResolveDir(%s, %q): %v", flag, in, err)
			}
			if got != in {
				t.Errorf("ResolveDir(%s, %q) = %q, want every byte preserved", flag, in, got)
			}
		}
	}
}

// EnsureReady must create (and target) dirs whose names are pure whitespace
// at the exact requested path.
func TestEnsureReadyWhitespaceNames(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{" leading", "trailing ", " "} {
		want := filepath.Join(base, name)
		if err := EnsureReady("-od", want, true); err != nil {
			t.Fatalf("EnsureReady(%q): %v", want, err)
		}
		if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
			t.Fatalf("dir %q not created (stat err=%v)", want, err)
		}
	}
}
