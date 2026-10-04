package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// testStamp is the shared fixture time for allocatePatternFiles tests.
var testStamp = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

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

func TestSlugifyWindowsReservedCharacters(t *testing.T) {
	got := slugify(`a/b\c:d*e?f"g<h>i|j`)
	for _, r := range `/\:*?"<>|` {
		if strings.ContainsRune(got, r) {
			t.Fatalf("slugify kept Windows-reserved character %q in %q", r, got)
		}
	}
	if got != "a_b_c_d_e_f_g_h_i_j" {
		t.Fatalf("slugify(adversarial pattern) = %q, want %q", got, "a_b_c_d_e_f_g_h_i_j")
	}
}

func TestPatternFileNameTrimsEdgeWhitespace(t *testing.T) {
	got := patternFileName("  lead  trail  ", "20260101-0000")
	if got != "lead  trail_20260101-0000.txt" {
		t.Fatalf("patternFileName() = %q, want %q", got, "lead  trail_20260101-0000.txt")
	}
}

func TestSlugifyLengthCap(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := slugify(long)
	if len(got) > slugByteBudget {
		t.Fatalf("slug len = %d, cap %d", len(got), slugByteBudget)
	}
}

// TestSlugifyCapsByUTF8Bytes pins the H-17 fix: the cap is encoded bytes, not
// runes, so a term of 64 four-byte runes (256 raw bytes) is trimmed to a name
// that fits NAME_MAX, the trim lands on a rune boundary, and the resulting
// per-pattern output file can actually be created and read back.
func TestSlugifyCapsByUTF8Bytes(t *testing.T) {
	term := strings.Repeat("😀", 64) // 64 runes = 256 UTF-8 bytes
	slug := slugify(term)
	if len(slug) > slugByteBudget {
		t.Fatalf("slug = %d bytes, cap %d", len(slug), slugByteBudget)
	}
	if !utf8.ValidString(slug) {
		t.Fatalf("slug %q is not valid UTF-8 (trim split a rune)", slug)
	}
	name := patternFileName(term, "20260927-2024")
	if len(name) > 255 {
		t.Fatalf("name %q is %d bytes, exceeds NAME_MAX 255", name, len(name))
	}

	// End to end: allocating the per-pattern output file must succeed.
	files, paths, err := allocatePatternFiles(t.TempDir(), []string{term}, testStamp)
	if err != nil {
		t.Fatalf("allocatePatternFiles: %v", err)
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("read back %s: %v", paths[0], err)
	}
	if len(data) != 0 {
		t.Fatalf("fresh output file not empty: %q", data)
	}
}

// TestSlugifyASCIIUnchanged pins that the byte-based cap does not alter ASCII
// behavior: for ASCII, byte trimming and rune trimming are identical, so a
// capped ASCII slug is exactly the same string the rune cap produced before.
func TestSlugifyASCIIUnchanged(t *testing.T) {
	if got, want := slugify("user@example.com"), "user@example_com"; got != want {
		t.Fatalf("slugify(ascii) = %q, want %q", got, want)
	}
	got := slugify(strings.Repeat("a", 300))
	if want := strings.Repeat("a", slugByteBudget); got != want {
		t.Fatalf("slugify(300 a's) len = %d, want %d (byte cap == rune cap for ASCII)", len(got), len(want))
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
	// "a.b" and "a:b" both slug to "a_b" -> collision suffix _2
	patterns := []string{"a.b", "a:b", "a.b"}
	files, paths, err := allocatePatternFiles(dir, patterns, testStamp)
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
func TestAllocatePatternFilesSecondRunPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	patterns := []string{"a.b", "a:b"}
	first, firstPaths, err := allocatePatternFiles(dir, patterns, testStamp)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range first {
		if _, err := f.WriteString("first-" + string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	second, secondPaths, err := allocatePatternFiles(dir, patterns, testStamp)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range second {
		_ = f.Close()
	}
	wantNames := []string{"a_b_20260102-0304_3.txt", "a_b_20260102-0304_4.txt"}
	for i, path := range secondPaths {
		if filepath.Base(path) != wantNames[i] {
			t.Fatalf("second path[%d] = %q, want %q", i, filepath.Base(path), wantNames[i])
		}
	}
	for i, path := range firstPaths {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := "first-" + string(rune('a'+i))
		if string(got) != want {
			t.Errorf("first file %q = %q, want %q", path, got, want)
		}
	}
}

func TestAllocatePatternFilesConcurrent(t *testing.T) {
	dir := t.TempDir()
	patterns := []string{"same", "same"}
	const runs = 8
	type result struct {
		paths []string
		err   error
	}
	results := make(chan result, runs)
	for i := 0; i < runs; i++ {
		go func() {
			files, paths, err := allocatePatternFiles(dir, patterns, testStamp)
			if err == nil {
				for _, f := range files {
					if _, writeErr := f.WriteString("x"); writeErr != nil && err == nil {
						err = writeErr
					}
					_ = f.Close()
				}
			}
			results <- result{paths: paths, err: err}
		}()
	}
	seen := make(map[string]bool, runs*len(patterns))
	for i := 0; i < runs; i++ {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		for _, path := range got.paths {
			if seen[path] {
				t.Fatalf("duplicate allocated path %q", path)
			}
			seen[path] = true
		}
	}
	if len(seen) != runs*len(patterns) {
		t.Fatalf("allocated %d paths, want %d", len(seen), runs*len(patterns))
	}
}

func TestAllocatePatternFilesCreatesDir(t *testing.T) {
	// validateFileOutputDir validates the -o path and ensureFileOutputDir
	// (called by main after the rejection checks) creates the dir;
	// allocatePatternFiles assumes it exists. Mirror that ordering here.
	dir := filepath.Join(t.TempDir(), "nested", "out")
	abs, err := validateFileOutputDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureFileOutputDir(abs); err != nil {
		t.Fatal(err)
	}
	files, _, err := allocatePatternFiles(dir, []string{"foo"}, testStamp)
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
	// Validation alone must not materialize anything; creation is a separate
	// step the caller runs after the rejection checks.
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("validateFileOutputDir must not create the dir; stat err = %v", err)
	}
	if err := ensureFileOutputDir(abs); err != nil {
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

// P5-W8: -f -o must preserve whitespace in quoted dir names — " leading" and
// "trailing " target exactly those bytes, and " " is a real (nonempty) dir
// name, not the empty flag.
func TestValidateFileOutputDirPreservesWhitespace(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{" leading", "trailing ", " "} {
		in := filepath.Join(base, name)
		abs, err := validateFileOutputDir(in)
		if err != nil {
			t.Fatalf("validateFileOutputDir(%q): %v", in, err)
		}
		if err := ensureFileOutputDir(abs); err != nil {
			t.Fatalf("ensureFileOutputDir(%q): %v", in, err)
		}
		if abs != in {
			t.Errorf("validateFileOutputDir(%q) abs = %q, want every byte preserved", in, abs)
		}
		if fi, statErr := os.Stat(in); statErr != nil || !fi.IsDir() {
			t.Fatalf("dir %q not created at exact path (stat err=%v)", in, statErr)
		}
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
		got, err := outputDirUnderRoot(c.outDir, c.root)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// Symlink identity, not lexical path shape, decides containment: an -o dir
// reached through a link pointing OUT of the root is allowed, an external
// path that resolves back INTO the root is rejected, and an existing path
// that cannot be resolved must fail closed with an error (usage error), not
// be silently treated as outside.
func TestOutputDirUnderRootSymlink(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()

	t.Run("out link to external dir allowed", func(t *testing.T) {
		link := filepath.Join(root, "out-link")
		if err := os.Symlink(external, link); err != nil {
			t.Fatal(err)
		}
		got, err := outputDirUnderRoot(link, root)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Errorf("out-link to external dir reported under root")
		}
	})

	t.Run("external alias resolving inside root rejected", func(t *testing.T) {
		alias := filepath.Join(external, "alias_"+filepath.Base(root))
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
		// nonexistent leaf under the symlinked root still resolves into root
		requested := filepath.Join(alias, "new-out")
		got, err := outputDirUnderRoot(requested, root)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Errorf("external path resolving inside root reported outside")
		}
	})

	t.Run("alias of root itself rejected", func(t *testing.T) {
		alias := filepath.Join(external, "root_alias_"+filepath.Base(root))
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
		got, err := outputDirUnderRoot(alias, root)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Errorf("symlink alias of root itself reported outside")
		}
	})

	t.Run("unresolvable existing path fails closed", func(t *testing.T) {
		loop := filepath.Join(root, "loop")
		if err := os.Symlink(loop, loop); err != nil {
			t.Fatal(err)
		}
		_, err := outputDirUnderRoot(loop, root)
		if err == nil {
			t.Fatalf("symlink loop must fail closed, got nil error")
		}
	})

	t.Run("dangling link target under root rejected", func(t *testing.T) {
		// The link itself sits outside root, so the lexical check on the link
		// name alone passes — but its (dangling) target resolves into root,
		// where materialized outputs would be discovered as inputs.
		link := filepath.Join(external, "dangling_out")
		if err := os.Symlink(filepath.Join(root, "out"), link); err != nil {
			t.Fatal(err)
		}
		got, err := outputDirUnderRoot(link, root)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Errorf("dangling -o link with target under root reported outside")
		}
	})

	t.Run("dangling relative link target under root rejected", func(t *testing.T) {
		// Relative targets must resolve against the link's directory, not cwd.
		link := filepath.Join(external, "dangling_rel_out")
		rel, err := filepath.Rel(external, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(rel, "out"), link); err != nil {
			t.Fatal(err)
		}
		got, err := outputDirUnderRoot(link, root)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Errorf("dangling -o link with relative target under root reported outside")
		}
	})

	t.Run("dangling link target outside root allowed", func(t *testing.T) {
		// Dangling target genuinely outside root stays allowed.
		link := filepath.Join(external, "dangling_external_out")
		if err := os.Symlink(filepath.Join(external, "missing-out"), link); err != nil {
			t.Fatal(err)
		}
		got, err := outputDirUnderRoot(link, root)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Errorf("dangling -o link with target outside root reported under root")
		}
	})
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
