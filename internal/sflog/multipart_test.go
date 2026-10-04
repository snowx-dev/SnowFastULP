package sflog

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// writeParts writes each blob to dir/name and returns the ordered paths, so a
// test can build a split set out of arbitrary chunk sizes.
func writeParts(t *testing.T, dir string, parts map[string][]byte, order []string) []string {
	t.Helper()
	out := make([]string, 0, len(order))
	for _, name := range order {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, parts[name], 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestMultiPartReaderAtConcatenation(t *testing.T) {
	dir := t.TempDir()
	a := []byte("the quick brown ")
	b := []byte("fox jumps over ")
	c := []byte("the lazy dog!!!")
	want := append(append(append([]byte{}, a...), b...), c...)
	paths := writeParts(t, dir,
		map[string][]byte{"s.001": a, "s.002": b, "s.003": c},
		[]string{"s.001", "s.002", "s.003"})

	ra, err := openMultiPartReaderAt(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer ra.Close()

	if ra.Size() != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", ra.Size(), len(want))
	}

	// Full read into an exactly-sized buffer succeeds with no error (the buffer
	// is filled even though it ends at EOF).
	full := make([]byte, len(want))
	n, err := ra.ReadAt(full, 0)
	if err != nil {
		t.Fatalf("ReadAt(full): n=%d err=%v", n, err)
	}
	if !bytes.Equal(full, want) {
		t.Fatalf("full read = %q, want %q", full, want)
	}

	// A read that straddles every boundary returns the right window.
	for _, tc := range []struct {
		off, n int
	}{
		{0, 5}, {14, 6}, {15, 1}, {16, 10}, {len(a), len(b)}, {len(want) - 3, 3},
	} {
		got := make([]byte, tc.n)
		rn, rerr := ra.ReadAt(got, int64(tc.off))
		if rerr != nil || rn != tc.n {
			t.Fatalf("ReadAt(off=%d,n=%d): rn=%d err=%v", tc.off, tc.n, rn, rerr)
		}
		if !bytes.Equal(got, want[tc.off:tc.off+tc.n]) {
			t.Fatalf("ReadAt(off=%d,n=%d) = %q, want %q", tc.off, tc.n, got, want[tc.off:tc.off+tc.n])
		}
	}

	// A short read at EOF must return what it can plus io.EOF (ReaderAt contract).
	tail := make([]byte, 10)
	rn, rerr := ra.ReadAt(tail, int64(len(want)-3))
	if rn != 3 || rerr != io.EOF {
		t.Fatalf("short read at EOF: n=%d err=%v, want 3/EOF", rn, rerr)
	}
	if !bytes.Equal(tail[:3], want[len(want)-3:]) {
		t.Fatalf("short read = %q, want %q", tail[:3], want[len(want)-3:])
	}

	// Reading at/over the end is immediate EOF.
	if n, err := ra.ReadAt(make([]byte, 4), ra.Size()); n != 0 || err != io.EOF {
		t.Fatalf("ReadAt at size: n=%d err=%v, want 0/EOF", n, err)
	}
}

func TestMultiPartReaderAtErrors(t *testing.T) {
	if _, err := openMultiPartReaderAt(nil); err == nil {
		t.Fatal("openMultiPartReaderAt(nil) should error")
	}
	dir := t.TempDir()
	paths := writeParts(t, dir, map[string][]byte{"x.001": []byte("hi")}, []string{"x.001"})
	paths = append(paths, filepath.Join(dir, "missing.002"))
	if _, err := openMultiPartReaderAt(paths); err == nil {
		t.Fatal("openMultiPartReaderAt with a missing part should error")
	}
}
