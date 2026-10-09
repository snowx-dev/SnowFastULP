package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// debugLogger writes structured, timestamped events to a log file when -debug
// is set. It records provenance (source path + credential counts) but never raw
// credential values.
type debugLogger struct {
	mu sync.Mutex
	f  *os.File
}

// logDir resolves where per-run log files (debug, issues) are written: the -o
// output dir, else the -od library dir, else the current directory.
func logDir(cfg runConfig) string {
	if cfg.OutputDir != "" {
		return cfg.OutputDir
	}
	// M-09: a dry-run consult (-odr, or [sfl] odr=true) is read-only by
	// contract; debug/ingest-debug/reject artifacts must not land inside a
	// library that is only being inspected. The CWD fallback mirrors
	// issueLogDir's dry-run behavior.
	if cfg.LibraryDir != "" && !cfg.DryRun {
		return cfg.LibraryDir
	}
	return "."
}

func newDebugLogger(cfg runConfig) *debugLogger {
	if !cfg.Debug {
		return nil
	}
	dir := logDir(cfg)
	_ = os.MkdirAll(dir, 0o755)
	started := cfg.Started
	if started.IsZero() {
		started = time.Now()
	}
	// os.CreateTemp with a random suffix mirrors the issues log: two runs
	// started in the same second (concurrent runs sharing one -od dir) would
	// otherwise clobber each other's sfl_debug_<timestamp>.log. RunStamp is
	// already unique per run; the random suffix additionally covers tests and
	// any caller that skips ensureRunStamp.
	stamp := cfg.RunStamp
	if stamp == "" {
		stamp = started.Format("20060102_150405")
	}
	f, err := os.CreateTemp(dir, "sfl_debug_"+stamp+"-*.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sfl: debug log disabled: %v\n", err)
		return nil
	}
	d := &debugLogger{f: f}
	d.Event("sfl debug log started")
	return d
}

func (d *debugLogger) Event(format string, args ...any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// Header records run provenance so the log is self-describing: what was run,
// where, and with which knobs. Paths and counts only, never raw credentials.
func (d *debugLogger) Header(cfg runConfig, passwords int, outPath string) {
	if d == nil {
		return
	}
	mode := "classic (-o " + cfg.OutputDir + ")"
	if cfg.LibraryDir != "" {
		mode = "ingest (-od " + cfg.LibraryDir + ")"
	}
	d.Event("version=%s gomaxprocs=%d", version.String, runtime.GOMAXPROCS(0))
	d.Event("config: input=%q mode=%q workers=%d passwords=%d noURI=%v compress=%v del=%v tempDir=%q",
		cfg.Input, mode, cfg.Workers, passwords, cfg.NoURI, cfg.Compress, cfg.DeleteSources, cfg.TempDir)
	d.Event("output: %s", outPath)
}

// Completion records the final aggregate outcome so a run can be assessed at a
// glance without re-deriving counts from the per-source lines above it.
func (d *debugLogger) Completion(stats sflog.ExtractStats) {
	if d == nil {
		return
	}
	d.Event("complete: logs=%d files=%d archives=%d credentials=%d emitted=%d duplicates=%d "+
		"skippedFiles=%d skippedArchives=%d passwordNotFound=%d parseIssues=%d openIssues=%d noULP=%d",
		stats.Logs, stats.FilesScanned, stats.ArchivesScanned, stats.Credentials, stats.Emitted, stats.Duplicates,
		stats.SkippedFiles, stats.SkippedArchives, stats.PasswordNotFound, stats.ParseErrors, stats.OpenErrors, stats.NoULP)
	d.Event("ambiguity total=%d path_or_password=%d port_or_login=%d", stats.AmbiguityTotal, stats.AmbiguityPathOrPassword, stats.AmbiguityPortOrLogin)
}

// Issues logs each recorded issue with path, kind, and detail for post-run review.
func (d *debugLogger) Issues(stats sflog.ExtractStats) {
	if d == nil || len(stats.Issues) == 0 {
		return
	}
	d.Event("issues: %d example(s) recorded (parse=%d open=%d password=%d volume=%d noULP=%d)",
		len(stats.Issues), stats.ParseErrors, stats.OpenErrors, stats.PasswordNotFound, stats.MissingVolumes, stats.NoULP)
	for _, is := range stats.Issues {
		// Keep the existing debug wording; the fuller explanation belongs in
		// the issue TSV.
		detail := ""
		if is.Kind != sflog.IssueMixedFormat {
			detail = sflog.IssueDetail(is)
		}
		if detail != "" {
			d.Event("  %s path=%q detail=%q", is.Kind, is.Path, detail)
		} else {
			d.Event("  %s path=%q", is.Kind, is.Path)
		}
	}
}

func (d *debugLogger) Close() {
	if d == nil || d.f == nil {
		return
	}
	_ = d.f.Close()
}

// issueLogResult reports the outcome of the automatic issue log: where the
// temp file landed (Path, empty unless issues were recorded and the close
// succeeded), how many issues were seen (Count), how many of those were
// top-level archives with no matching password (TopLevelPasswordNotFound),
// and the first create/write/close error (Err).
type issueLogResult struct {
	Path                     string
	Count                    int
	TopLevelPasswordNotFound int
	Err                      error
}

// issueLogDir returns the stable directory for the automatic issue log. For a
// real run it mirrors the -debug log's location exactly (logDir: -o output
// dir, else -od library dir, else CWD) so failure evidence survives reboots —
// a platform temp dir (TMPDIR) is wiped and the log was previously lost with
// it. A dry-run consult (-odr, or [sfl] odr=true) is read-only by contract:
// the library dir is excluded there and the log lands in the CWD fallback
// instead, so previewing a library never leaves a file inside it. The
// platform temp dir remains a last-resort fallback for create failures.
func issueLogDir(cfg runConfig) string {
	if cfg.DryRun {
		return "."
	}
	return logDir(cfg)
}

// createIssueTemp is a package-level seam so tests can inject temp-file
// creation failures.
var createIssueTemp = func(pattern string) (*os.File, error) {
	return os.CreateTemp("", pattern)
}

// issueLogger streams every issue (untruncated, unlike the capped summary) to
// a dedicated log, replacing the old opt-in -err file. The file is created
// lazily on the first recorded issue — a clean run creates nothing — in the
// stable location next to the -debug log (issueLogDir = logDir: -o output
// dir, else -od library dir, else CWD) so failure evidence survives reboots;
// a platform temp dir (TMPDIR) is wiped and took the log with it. When the
// stable location cannot be created, it falls back to the platform temp dir.
// Mode is 0600 where permissions are supported. nil remains a safe no-op for
// all methods.
type issueLogger struct {
	mu           sync.Mutex
	cfg          runConfig
	dir          string
	f            *os.File
	count        int
	topLevelNoPW []string // first-seen top-level password-not-found paths
	topLevelSeen map[string]struct{}
	err          error // first create/write/close error; sticky
	closed       bool
	res          issueLogResult
}

// newIssueLogger returns an always-wired lazy logger. No file is created here;
// the first Record creates one named after the run stamp.
func newIssueLogger(cfg runConfig) *issueLogger {
	return &issueLogger{cfg: cfg, dir: issueLogDir(cfg)}
}

// create opens the issue log in the stable dir when one applies, falling back
// to the platform temp dir (createIssueTemp seam) when there is none or the
// stable location cannot be created.
func (l *issueLogger) create(pattern string) (*os.File, error) {
	if l.dir != "" {
		if err := os.MkdirAll(l.dir, 0o755); err == nil {
			if f, ferr := os.CreateTemp(l.dir, pattern); ferr == nil {
				return f, nil
			}
		}
	}
	return createIssueTemp(pattern)
}

// Record streams one issue as it happens, creating the temp log on first use.
// Safe for concurrent worker calls; the mutex only guards this file, never the
// extraction path. After a create/write failure the error is stored once and
// writes stop retrying, while incoming events keep being counted.
// Top-level password-not-found paths (no "!" nest marker) are remembered for
// the close-time preamble and the Issues footer count.
func (l *issueLogger) Record(path string, kind sflog.IssueKind, err error) {
	if l == nil {
		return
	}
	line := fmt.Sprintf("%s\t%s", kind, path)
	if detail := sflog.IssueDetail(sflog.Issue{Path: path, Kind: kind, Err: err}); detail != "" {
		line += "\t" + detail
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
	if kind == sflog.IssuePasswordNotFound && !strings.Contains(path, "!") {
		if l.topLevelSeen == nil {
			l.topLevelSeen = make(map[string]struct{})
		}
		if _, dup := l.topLevelSeen[path]; !dup {
			l.topLevelSeen[path] = struct{}{}
			l.topLevelNoPW = append(l.topLevelNoPW, path)
		}
	}
	if l.err != nil {
		return
	}
	if l.f == nil {
		f, ferr := l.create("sfl-issues-" + l.cfg.RunStamp + "-*.log")
		if ferr != nil {
			l.err = ferr
			return
		}
		l.f = f
		started := startedOrNow(l.cfg)
		if _, herr := fmt.Fprintf(f, "# sfl issues — %s\n# kind\tpath\tdetail\n", started.Format("2006-01-02 15:04:05")); herr != nil {
			// Same sticky semantics as any other write failure: the error is
			// stored once and later records only count.
			l.err = herr
			return
		}
	}
	if _, werr := fmt.Fprintln(l.f, line); werr != nil {
		l.err = werr
	}
}

// Close flushes the total footer, prepends any top-level password-miss list,
// and closes the file, returning the stored result. Idempotent: repeated
// closes return the same result. A run with zero issues never created a file
// and closes to a zero result.
func (l *issueLogger) Close() issueLogResult {
	if l == nil {
		return issueLogResult{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.res
	}
	l.closed = true
	l.res.Count = l.count
	l.res.TopLevelPasswordNotFound = len(l.topLevelNoPW)
	if l.err != nil {
		l.res.Err = l.err
		if l.f != nil {
			_ = l.f.Close()
			l.f = nil
		}
		return l.res
	}
	if l.f == nil {
		return l.res
	}
	name := l.f.Name()
	if _, werr := fmt.Fprintf(l.f, "# %d issue(s) total\n", l.count); werr != nil {
		l.res.Err = werr
	}
	if cerr := l.f.Close(); l.res.Err == nil {
		l.res.Err = cerr
	}
	l.f = nil
	if l.res.Err == nil && len(l.topLevelNoPW) > 0 {
		l.res.Err = rewriteIssueLogWithTopLevel(name, l.topLevelNoPW)
	}
	if l.res.Err == nil {
		l.res.Path = name
	}
	return l.res
}

// rewriteIssueLogWithTopLevel inserts a commented list of top-level archives
// that had no matching password immediately under the stamp header so the
// analyst can find them without scanning the TSV body.
func rewriteIssueLogWithTopLevel(path string, topLevel []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(data)
	nl := strings.IndexByte(text, '\n')
	if nl < 0 {
		return fmt.Errorf("issue log missing header newline")
	}
	var b strings.Builder
	b.Grow(len(text) + 64*len(topLevel))
	b.WriteString(text[:nl+1])
	b.WriteString("# top-level archives with no correct password:\n")
	for _, p := range topLevel {
		b.WriteString("#   ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString(text[nl+1:])
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
