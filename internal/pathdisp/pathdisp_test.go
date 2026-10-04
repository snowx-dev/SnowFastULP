package pathdisp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/pathdisp"
)

func TestForDisplayUnderCWD(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	got := pathdisp.ForDisplay("hits.txt")
	if got != "hits.txt" {
		t.Fatalf("got %q, want hits.txt", got)
	}
	nested := filepath.Join("sub", "out.txt")
	if err := os.MkdirAll("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	got = pathdisp.ForDisplay(nested)
	if got != nested {
		t.Fatalf("got %q, want %q", got, nested)
	}
}

func TestForDisplayOutsideCWD(t *testing.T) {
	work := t.TempDir()
	other := t.TempDir()
	t.Chdir(work)
	abs := filepath.Join(other, "away.txt")
	got := pathdisp.ForDisplay(abs)
	want, err := filepath.Abs(abs)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %q, want absolute %q", got, want)
	}
}

func TestForDisplayNotesUnchanged(t *testing.T) {
	if got := pathdisp.ForDisplay("(no matches)"); got != "(no matches)" {
		t.Fatalf("got %q", got)
	}
	if got := pathdisp.ForDisplay(""); got != "" {
		t.Fatalf("got %q", got)
	}
}
