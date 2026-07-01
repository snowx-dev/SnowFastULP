package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

func newDebugLogger(cfg runConfig) *debugLogger {
	if !cfg.Debug {
		return nil
	}
	dir := cfg.OutputDir
	if dir == "" {
		dir = cfg.LibraryDir
	}
	if dir == "" {
		dir = "."
	}
	_ = os.MkdirAll(dir, 0o755)
	started := cfg.Started
	if started.IsZero() {
		started = time.Now()
	}
	name := filepath.Join(dir, "sfl_debug_"+started.Format("20060102_150405")+".log")
	f, err := os.Create(name)
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
}

// Issues logs each recorded issue with path, kind, and detail for post-run review.
func (d *debugLogger) Issues(stats sflog.ExtractStats) {
	if d == nil || len(stats.Issues) == 0 {
		return
	}
	d.Event("issues: %d example(s) recorded (parse=%d open=%d password=%d volume=%d noULP=%d)",
		len(stats.Issues), stats.ParseErrors, stats.OpenErrors, stats.PasswordNotFound, stats.MissingVolumes, stats.NoULP)
	for _, is := range stats.Issues {
		detail := sflog.IssueDetail(is)
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
