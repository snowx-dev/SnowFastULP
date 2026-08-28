package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestLoadPatternsFileBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terms.txt")
	if err := os.WriteFile(path, []byte("foo\r\nbar\n\nbaz\r"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadPatternsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"foo", "bar", "baz"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLoadPatternsFileRejectsStar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terms.txt")
	if err := os.WriteFile(path, []byte("foo\n*\nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadPatternsFile(path)
	if err == nil || !strings.Contains(err.Error(), "'*'") {
		t.Fatalf("err = %v, want '*' rejection", err)
	}
}

func TestLoadPatternsFileMissing(t *testing.T) {
	_, err := loadPatternsFile(filepath.Join(t.TempDir(), "nope.txt"))
	if err == nil || !strings.Contains(err.Error(), "-f:") {
		t.Fatalf("err = %v, want -f: prefix", err)
	}
}

func TestLoadPatternsFileAllBlank(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terms.txt")
	if err := os.WriteFile(path, []byte("\n\r\n\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadPatternsFile(path)
	if err == nil || !strings.Contains(err.Error(), "no search terms") {
		t.Fatalf("err = %v, want no search terms", err)
	}
}

func TestLoadPatternsFileKeepsSpaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "terms.txt")
	if err := os.WriteFile(path, []byte("a b c\ndef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadPatternsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a b c" {
		t.Fatalf("got %v, want [a b c def]", got)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"user@example.com", "user@example_com"}, // '.' -> '_', '@' kept
		{"foo bar", "foo bar"},                   // space kept (not punctuation)
		{"a/b\\c", "a_b_c"},                      // path separators -> '_'
		{"a..b", "a_b"},                          // run collapse + trim
		{"!!!", ""},                              // symbol-only -> empty (caller adds stamp)
		{"hello", "hello"},
		{"a@b@c", "a@b@c"}, // multiple '@' kept
		{"a:b:c", "a_b_c"},
	}
	for _, c := range cases {
		if got := slugify(c.in); got != c.want {
			t.Fatalf("slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugifyLengthCap(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := slugify(long)
	if len([]rune(got)) > slugMaxLen {
		t.Fatalf("slug len = %d, cap %d", len([]rune(got)), slugMaxLen)
	}
}

func TestPatternFileNameEmptySlug(t *testing.T) {
	if got := patternFileName("!!!", "20260101-0000"); got != "pattern_20260101-0000.txt" {
		t.Fatalf("got %q", got)
	}
	if got := patternFileName("foo@bar", "20260101-0000"); got != "foo@bar_20260101-0000.txt" {
		t.Fatalf("got %q", got)
	}
}

func TestAllocatePatternFilesCollision(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// "a.b" and "a:b" both slug to "a_b" -> collision suffix _2
	patterns := []string{"a.b", "a:b", "a.b"}
	files, paths, err := allocatePatternFiles(dir, patterns, stamp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	bases := []string{
		"a_b_20260102-0304.txt",
		"a_b_20260102-0304_2.txt",
		"a_b_20260102-0304_3.txt",
	}
	for i, want := range bases {
		if filepath.Base(paths[i]) != want {
			t.Fatalf("path[%d] = %q, want %q", i, filepath.Base(paths[i]), want)
		}
	}
}

func TestAllocatePatternFilesCreatesDir(t *testing.T) {
	// validateFileOutputDir (called by main before allocate) creates the dir;
	// allocatePatternFiles assumes it exists. Mirror that ordering here.
	dir := filepath.Join(t.TempDir(), "nested", "out")
	if _, err := validateFileOutputDir(dir); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	files, _, err := allocatePatternFiles(dir, []string{"foo"}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files[0].Close() }()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("output dir not created: %v", dir)
	}
}

func TestValidateFileOutputDirRejectsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := validateFileOutputDir(file)
	if err == nil || !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("err = %v, want must be a directory", err)
	}
}

func TestValidateFileOutputDirCreatesMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "newdir")
	abs, err := validateFileOutputDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		t.Fatalf("dir not created at %s", abs)
	}
}

func TestValidateFileOutputDirEmpty(t *testing.T) {
	_, err := validateFileOutputDir("")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want empty rejection", err)
	}
}

func TestOutputDirUnderRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "out")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(root), "out_"+filepath.Base(root))
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		outDir string
		root   string
		want   bool
	}{
		{"same dir", root, root, true},
		{"nested inside", inside, root, true},
		{"sibling outside", outside, root, false},
		{"empty outDir", "", root, false},
		{"empty root", outside, "", false},
	}
	for _, c := range cases {
		if got := outputDirUnderRoot(c.outDir, c.root); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDispatchSinkRoutesByPatternIdx(t *testing.T) {
	// In-memory buffers stand in for per-pattern files.
	var bufs []*bytes.Buffer
	for i := 0; i < 3; i++ {
		bufs = append(bufs, &bytes.Buffer{})
	}
	// Build a dispatchSink over search.Writer wrapping each buffer.
	d := &dispatchSink{writers: make([]*search.Writer, len(bufs))}
	for i, b := range bufs {
		d.writers[i] = search.NewWriter(b, false)
	}
	hits := []search.Hit{
		{PatternIdx: 0, Line: "zero"},
		{PatternIdx: 2, Line: "two"},
		{PatternIdx: 1, Line: "one"},
		{PatternIdx: 0, Line: "zero2"},
	}
	for _, h := range hits {
		if err := d.WriteHit(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	if bufs[0].String() != "zero\nzero2\n" {
		t.Fatalf("buf0 = %q", bufs[0].String())
	}
	if bufs[1].String() != "one\n" {
		t.Fatalf("buf1 = %q", bufs[1].String())
	}
	if bufs[2].String() != "two\n" {
		t.Fatalf("buf2 = %q", bufs[2].String())
	}
}

func TestDispatchSinkOutOfRange(t *testing.T) {
	d := &dispatchSink{writers: []*search.Writer{search.NewWriter(&bytes.Buffer{}, false)}}
	err := d.WriteHit(search.Hit{PatternIdx: 5, Line: "x"})
	if err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("err = %v, want out of range", err)
	}
}

func TestDispatchSinkCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	d := newDispatchSink([]*os.File{f}, false)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d.files[0] != nil {
		t.Fatal("Close should nil the file slot")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}
