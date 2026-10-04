package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/snowx-dev/SnowFastULP/internal/outdir"
	"github.com/snowx-dev/SnowFastULP/internal/pathident"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

// loadPatternsFile reads a file of search terms (one per line), strips a
// trailing CRLF, skips blank lines, and rejects the "*" sentinel (match-all
// is a single-pattern CLI affordance, not meaningful in file mode). The
// returned slice preserves the user's order; duplicate terms are kept (the
// matcher dedups them on the trie, and each index still routes to its own
// output file so a repeated term yields two sibling files).
func loadPatternsFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if line == "*" {
			return nil, fmt.Errorf("-f: '*' is not allowed in file mode; pass it on the CLI instead")
		}
		patterns = append(patterns, line)
	}
	if len(patterns) == 0 {
		return nil, fmt.Errorf("-f: no search terms found in %s", path)
	}
	return patterns, nil
}

// slugByteBudget caps the slugified term portion of a per-pattern filename in
// encoded UTF-8 bytes, not runes: 64 four-byte runes are 256 bytes, which
// alone overflows the common NAME_MAX of 255 once the timestamp suffix and
// ".txt" extension are appended. The budget reserves room for the
// "_<stamp>.txt" suffix (18 bytes) and the worst-case collision suffix
// "_1000" (5), so the full name always fits.
const slugByteBudget = 255 - len("_20060102-1504.txt") - len("_1000")

// slugify turns a search term into a filename-safe slug: every rune that is a
// path separator, control char, Windows-reserved filename character, or
// punctuation becomes '_', except '@' which is preserved (the one punctuation
// symbol the user asked to keep). Runs of '_' collapse to one, leading/trailing
// '_' are trimmed, and the result is capped at slugByteBudget encoded bytes,
// trimming only at a rune boundary so no multibyte sequence is split. A purely-symbolic
// term (e.g. "!!!") slugs to "", which the caller disambiguates by appending a
// run timestamp + a collision suffix, so symbol-only terms never produce empty
// or clashing names.
func slugify(term string) string {
	var b strings.Builder
	b.Grow(len(term))
	prevUnder := false
	for _, r := range term {
		switch {
		case r == '@':
			b.WriteRune(r)
			prevUnder = false
		case r == '/' || r == '\\' || r == '<' || r == '>' || r == '|' || unicode.IsControl(r) || unicode.IsPunct(r):
			if !prevUnder {
				b.WriteByte('_')
				prevUnder = true
			}
		default:
			b.WriteRune(r)
			prevUnder = false
		}
	}
	s := strings.Trim(b.String(), "_")
	// Cap by encoded UTF-8 bytes, not runes (rune counts don't bound the
	// on-disk name length), trimming only at a rune boundary so no
	// multibyte sequence is split.
	if len(s) > slugByteBudget {
		cut := slugByteBudget
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimRight(s[:cut], "_")
	}
	return s
}

// patternFileName builds the per-pattern output filename: <slug>_<stamp>.txt,
// where stamp is YYYYMMDD-HHMM. An empty slug (symbol-only term) becomes
// "pattern" so the file is still self-describing. The caller resolves
// collisions via allocatePatternFiles.
func patternFileName(term, stamp string) string {
	slug := strings.TrimSpace(slugify(term))
	if slug == "" {
		slug = "pattern"
	}
	return slug + "_" + stamp + ".txt"
}

// maxPatternFileSuffix bounds the on-disk collision search. Suffix 0 is the
// unsuffixed base name; suffixes 2 through 999 are then available.
const maxPatternFileSuffix = 999

// prospectivePatternFilePaths computes every per-pattern output path -f mode
// is about to create under dir, without touching the filesystem: each
// pattern's base name (patternFileName) plus, for bases claimed by more than
// one pattern, the _2.._N collision suffixes allocatePatternFiles may pick.
// The -json preflight compares its target against this set BEFORE
// allocation runs, because allocation itself truncates the files it opens.
func prospectivePatternFilePaths(dir string, patterns []string, stamp time.Time) []string {
	stampStr := stamp.Format("20060102-1504")
	counts := make(map[string]int, len(patterns))
	for _, term := range patterns {
		counts[patternFileName(term, stampStr)]++
	}
	paths := make([]string, 0, len(patterns))
	for base, n := range counts {
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		for i := range n {
			name := base
			if i > 0 {
				name = fmt.Sprintf("%s_%d%s", stem, i+1, ext)
			}
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths
}

// allocatePatternFiles opens one file per pattern under dir, slugifying each
// term and exclusively suffixing _2, _3, ... on name clashes (two terms that
// slug to the same base, e.g. "a.b" and "a:b"). Returns the open files
// (indexed parallel to patterns) and their paths for the run summary. The
// caller owns closing the files (typically via the dispatchSink's Flush + a
// deferred close).
func allocatePatternFiles(dir string, patterns []string, stamp time.Time) ([]*os.File, []string, error) {
	stampStr := stamp.Format("20060102-1504")
	seen := make(map[string]bool, len(patterns))
	files := make([]*os.File, len(patterns))
	paths := make([]string, len(patterns))
	for i, term := range patterns {
		base := patternFileName(term, stampStr)
		for suffix := 0; suffix < maxPatternFileSuffix; suffix++ {
			name := base
			if suffix > 0 {
				ext := filepath.Ext(base)
				stem := strings.TrimSuffix(base, ext)
				name = fmt.Sprintf("%s_%d%s", stem, suffix+1, ext)
			}
			if seen[name] {
				continue
			}
			path := filepath.Join(dir, name)
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				if errors.Is(err, fs.ErrExist) {
					continue
				}
				cleanupPatternFiles(files, paths)
				return nil, nil, fmt.Errorf("-f: create output %s: %w", path, err)
			}
			seen[name] = true
			files[i] = f
			paths[i] = path
			break
		}
		if files[i] == nil {
			cleanupPatternFiles(files, paths)
			return nil, nil, fmt.Errorf("-f: no available output filename for %s (suffix limit %d)", base, maxPatternFileSuffix)
		}
	}
	return files, paths, nil
}

func cleanupPatternFiles(files []*os.File, paths []string) {
	for i, f := range files {
		if f != nil {
			_ = f.Close()
			if paths[i] != "" {
				_ = os.Remove(paths[i])
			}
		}
	}
}

// validateFileOutputDir checks that an -o path in -f mode is usable as a
// directory: an existing file is rejected with a plain message (no raw
// ENOTDIR later). It resolves the path but does NOT create missing parents;
// the caller materializes the directory with ensureFileOutputDir only after
// every rejection check (notably the under-root nesting check) has passed, so
// a rejected invocation never leaves a directory behind on disk. Unlike
// outdir.ResolveDir's -o branch it does NOT require a trailing-slash dir hint
// (Q12: be flexible), accepting a plain path that we then ensure is a
// directory.
func validateFileOutputDir(o string) (string, error) {
	// No TrimSpace: whitespace in a quoted -o path is filesystem data (P5-W8).
	// Only "" is the empty flag; every nonempty path is preserved verbatim.
	if o == "" {
		return "", fmt.Errorf("-f: -o must point to a directory (got empty path)")
	}
	abs, err := filepath.Abs(o)
	if err != nil {
		return "", fmt.Errorf("-f: resolve -o: %w", err)
	}
	if fi, statErr := os.Stat(abs); statErr == nil && !fi.IsDir() {
		return "", fmt.Errorf("-o must be a directory in -f mode (got existing file): %s", abs)
	}
	// A bare nonexistent -o auto-creates as a directory, so a file-like name
	// is rejected up front instead of silently becoming one (resfile.txt/).
	// A trailing separator is always dir intent (escape hatch for names that
	// contain a dot); existing paths are exempt (a dir named v1.2 is fine);
	// leading-dot names (.cache) are not file-like.
	if _, statErr := os.Stat(abs); statErr != nil &&
		!strings.HasSuffix(o, "/") && !strings.HasSuffix(o, `\`) {
		if dot := strings.LastIndex(filepath.Base(o), "."); dot > 0 {
			return "", fmt.Errorf("-f: -o looks like a file name; add a trailing / to create it as a directory: %s", o)
		}
	}
	return abs, nil
}

// validateSingleOutputFile rejects directory-shaped targets for the
// single-pattern/-stats -o, which always names a FILE. Checks run before
// any parent creation so a rejected spelling leaves nothing behind. The
// raw suffix is checked first: filepath.Clean destroys a trailing separator.
func validateSingleOutputFile(o string) error {
	if strings.HasSuffix(o, "/") || strings.HasSuffix(o, `\`) {
		return fmt.Errorf("-o names a file in this mode; got a directory path: %s", o)
	}
	abs, err := filepath.Abs(o)
	if err != nil {
		return fmt.Errorf("resolve -o: %w", err)
	}
	if fi, statErr := os.Stat(abs); statErr == nil && fi.IsDir() {
		return fmt.Errorf("-o names a file in this mode; got an existing directory: %s", abs)
	}
	return nil
}

// ensureFileOutputDir materializes the validated -o directory (missing
// parents created) by reusing outdir.EnsureReady for the exists-and-not-a-dir
// + MkdirAll pair so error wording stays consistent with sfl/sfu. Called only
// after all rejection checks have passed.
func ensureFileOutputDir(abs string) error {
	return outdir.EnsureReady("-o", abs, true)
}

// outputDirUnderRoot reports whether outDir is the same as or nested inside
// root, judged by filesystem identity rather than lexical path shape: both
// paths are canonicalized through their deepest existing ancestor (so
// symlinked parents resolve), the two directories are compared with
// os.SameFile for equality, and only then is filepath.Rel applied to the
// canonical paths. In -f + -o DIR mode the per-pattern files are created
// before discovery, so a dir under the search root would be discovered and
// searched (and in -txt mode those .txt outputs would be scanned mid-write).
// Rejecting the nesting up front keeps the run self-consistent; the single
// -o path is guarded by ensureNoOutputCollision against archive collisions.
// An existing path that cannot be statted or resolved (e.g. a symlink loop,
// an unreadable ancestor) fails closed with an error instead of being
// treated as outside; the caller must turn that into a usage error.
func outputDirUnderRoot(outDir, root string) (bool, error) {
	return outputDirUnderRootN(outDir, root, 0)
}

// maxSymlinkDepth bounds target resolution of symlinked -o paths; anything
// deeper is treated as a loop and fails closed.
const maxSymlinkDepth = 8

func outputDirUnderRootN(outDir, root string, depth int) (bool, error) {
	if outDir == "" || root == "" {
		return false, nil
	}
	// Identity check first: the same directory reached by two different
	// paths (alias, mount) is under the root regardless of spelling.
	if outInfo, err := os.Stat(outDir); err == nil {
		if rootInfo, rerr := os.Stat(root); rerr == nil && os.SameFile(outInfo, rootInfo) {
			return true, nil
		}
	}
	// A dangling -o symlink evades the canonicalization below: the link
	// itself does not stat, so its name is preserved as a suffix under the
	// link's parent while its TARGET is never consulted. A link outside root
	// whose target resolves under root would thus pass, and once the target
	// is materialized its outputs get discovered as inputs. Judge the
	// resolved target (absolute or relative to the link) too.
	if fi, lerr := os.Lstat(outDir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		raw, rerr := os.Readlink(outDir)
		if rerr != nil {
			return false, fmt.Errorf("resolve -o directory %s: %w", outDir, rerr)
		}
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(filepath.Dir(outDir), raw)
		}
		if depth >= maxSymlinkDepth {
			return false, fmt.Errorf("resolve -o directory %s: too many symlink levels (possible loop)", outDir)
		}
		targetUnder, terr := outputDirUnderRootN(filepath.Clean(raw), root, depth+1)
		if terr != nil {
			return false, fmt.Errorf("resolve -o directory %s: %w", outDir, terr)
		}
		if targetUnder {
			return true, nil
		}
	}
	canonicalOut, err := pathident.CanonicalProspective(outDir)
	if err != nil {
		return false, fmt.Errorf("resolve -o directory %s: %w", outDir, err)
	}
	canonicalRoot, err := pathident.CanonicalProspective(root)
	if err != nil {
		return false, fmt.Errorf("resolve search root %s: %w", root, err)
	}
	if canonicalOut == canonicalRoot {
		return true, nil
	}
	rel, err := filepath.Rel(canonicalRoot, canonicalOut)
	if err != nil {
		return false, fmt.Errorf("relate -o %s to search root %s: %w", outDir, root, err)
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// hitSink is the per-hit destination abstraction shared by single-pattern and
// multi-pattern modes. search.Writer (single stream/tee/file) and dispatchSink
// (per-pattern files) both satisfy it, so run() handles both uniformly.
type hitSink interface {
	WriteHit(search.Hit) error
	Flush() error
}

// dispatchSink routes hits to per-pattern output writers by Hit.PatternIdx.
// One search.Writer wraps each open per-pattern file; WriteHit selects the
// writer for the hit's pattern and Flush fans out to all of them. Used only
// in -f + -o DIR mode (file-only, no stdout); -f without -o streams all hits
// to a single stdout writer instead.
type dispatchSink struct {
	writers []*search.Writer
	files   []*os.File
}

// newDispatchSink wraps one file per pattern (parallel to the patterns slice)
// in a search.Writer. The caller is responsible for closing the files after
// the run (Flush is idempotent-ish: it just flushes buffers).
func newDispatchSink(files []*os.File, clean bool) *dispatchSink {
	writers := make([]*search.Writer, len(files))
	for i, f := range files {
		writers[i] = search.NewWriter(f, clean)
	}
	return &dispatchSink{writers: writers, files: files}
}

func (d *dispatchSink) WriteHit(h search.Hit) error {
	if h.PatternIdx < 0 || h.PatternIdx >= len(d.writers) {
		return fmt.Errorf("dispatchSink: PatternIdx %d out of range (have %d writers)", h.PatternIdx, len(d.writers))
	}
	return d.writers[h.PatternIdx].WriteHit(h)
}

// Flush flushes every per-pattern writer. Returns the first error encountered;
// subsequent flushes still run so a single bad file doesn't strand the rest.
func (d *dispatchSink) Flush() error {
	var first error
	for _, w := range d.writers {
		if err := w.Flush(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// printMultiPatternSummary writes one stderr line per pattern output file:
// "N hits → path" for files that received hits, and a trailing total. The
// per-file hit counts come from the dispatchSink's writers (each writer's
// hit count is tracked via the shared metrics.Hits total; per-file counts are
// not maintained separately to keep the sink allocation-free, so we report
// the total against the files that exist). This mirrors the single-pattern
// summary line for visual consistency.
func printMultiPatternSummary(totalHits int64, paths []string) {
	if len(paths) == 0 {
		return
	}
	for _, p := range paths {
		fmt.Fprintf(os.Stderr, "→ %s\n", p)
	}
	noun := "hits"
	if totalHits == 1 {
		noun = "hit"
	}
	filesNoun := "files"
	if len(paths) == 1 {
		filesNoun = "file"
	}
	fmt.Fprintf(os.Stderr, "%d %s across %d %s\n", totalHits, noun, len(paths), filesNoun)
}

// Close closes the underlying per-pattern files. Call after Flush at run end.
// Idempotent: a second close (e.g. test cleanup after run()'s own close) is a
// no-op so the interrupted-output defer and the success path can both call it.
func (d *dispatchSink) Close() error {
	var first error
	for i, f := range d.files {
		if f != nil {
			if err := f.Close(); err != nil && first == nil {
				first = err
			}
			d.files[i] = nil
		}
	}
	return first
}
