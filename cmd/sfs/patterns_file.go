package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/snowx-dev/SnowFastULP/internal/outdir"
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

// slugMaxLen caps the slugified term portion of a per-pattern filename so very
// long terms don't blow past filesystem name limits (the timestamp suffix is
// added after). 64 runes is comfortably under any common NAME_MAX.
const slugMaxLen = 64

// slugify turns a search term into a filename-safe slug: every rune that is a
// path separator, control char, or punctuation becomes '_', except '@' which
// is preserved (the one punctuation symbol the user asked to keep). Runs of
// '_' collapse to one, leading/trailing '_' are trimmed, and the result is
// capped at slugMaxLen runes. A purely-symbolic term (e.g. "!!!") slugs to "",
// which the caller disambiguates by appending a run timestamp + a collision
// suffix, so symbol-only terms never produce empty or clashing names.
func slugify(term string) string {
	var b strings.Builder
	b.Grow(len(term))
	prevUnder := false
	for _, r := range term {
		switch {
		case r == '@':
			b.WriteRune(r)
			prevUnder = false
		case r == '/' || r == '\\' || unicode.IsControl(r) || unicode.IsPunct(r):
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
	// Cap by runes, not bytes, to avoid splitting a multibyte sequence.
	if rs := []rune(s); len(rs) > slugMaxLen {
		s = string(rs[:slugMaxLen])
		s = strings.TrimRight(s, "_")
	}
	return s
}

// patternFileName builds the per-pattern output filename: <slug>_<stamp>.txt,
// where stamp is YYYYMMDD-HHMM. An empty slug (symbol-only term) becomes
// "pattern" so the file is still self-describing. The caller resolves
// collisions via allocatePatternFiles.
func patternFileName(term, stamp string) string {
	slug := slugify(term)
	if slug == "" {
		slug = "pattern"
	}
	return slug + "_" + stamp + ".txt"
}

// allocatePatternFiles opens one file per pattern under dir, slugifying each
// term and silently suffixing _2, _3, ... on name clashes (two terms that slug
// to the same base, e.g. "a.b" and "a:b"). Returns the open files (indexed
// parallel to patterns) and their paths for the run summary. The caller owns
// closing the files (typically via the dispatchSink's Flush + a deferred
// close).
func allocatePatternFiles(dir string, patterns []string, stamp time.Time) ([]*os.File, []string, error) {
	stampStr := stamp.Format("20060102-1504")
	seen := make(map[string]int, len(patterns))
	files := make([]*os.File, len(patterns))
	paths := make([]string, len(patterns))
	for i, term := range patterns {
		base := patternFileName(term, stampStr)
		name := base
		// Collision suffix: if this slug was already taken by an earlier
		// pattern, append _2, _3, ... until free. Mirrors the
		// sfs_results_<stamp>_N style used by defaultOutputPath.
		if n, taken := seen[base]; taken {
			for {
				ext := filepath.Ext(base)
				stem := strings.TrimSuffix(base, ext)
				name = fmt.Sprintf("%s_%d%s", stem, n, ext)
				if _, dup := seen[name]; !dup {
					break
				}
				n++
			}
			seen[name] = n + 1
		} else {
			seen[base] = 2
		}
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			// Close any already-opened files before returning so a partial
			// allocation doesn't leak handles.
			for _, op := range files {
				if op != nil {
					_ = op.Close()
				}
			}
			return nil, nil, fmt.Errorf("-f: create output %s: %w", path, err)
		}
		files[i] = f
		paths[i] = path
	}
	return files, paths, nil
}

// validateFileOutputDir checks that an -o path in -f mode is usable as a
// directory: an existing file is rejected with a plain message (no raw
// ENOTDIR later), and missing parents are created. Unlike outdir.ResolveDir's
// -o branch it does NOT require a trailing-slash dir hint (Q12: be flexible),
// accepting a plain path that we then ensure is a directory. Reuses
// outdir.EnsureReady for the exists-and-not-a-dir + MkdirAll pair so error
// wording stays consistent with sfl/sfu.
func validateFileOutputDir(o string) (string, error) {
	o = strings.TrimSpace(o)
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
	if err := outdir.EnsureReady("-o", abs, true); err != nil {
		return "", err
	}
	return abs, nil
}

// outputDirUnderRoot reports whether outDir (absolute) is the same as or nested
// inside root (absolute). In -f + -o DIR mode the per-pattern files are created
// before discovery, so a dir under the search root would be discovered and
// searched (and in -txt mode those .txt outputs would be scanned mid-write).
// Rejecting the nesting up front keeps the run self-consistent; the single
// -o path is guarded by ensureNoOutputCollision against archive collisions.
func outputDirUnderRoot(outDir, root string) bool {
	if outDir == "" || root == "" {
		return false
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if absOut == absRoot {
		return true
	}
	rel, err := filepath.Rel(absRoot, absOut)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
// the total against the files that exist). This mirrors printSecretsSummary
// in secrets_search.go for visual consistency.
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
	fmt.Fprintf(os.Stderr, "%d %s across %d files\n", totalHits, noun, len(paths))
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
