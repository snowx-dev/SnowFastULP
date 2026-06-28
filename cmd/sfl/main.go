package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/console"
	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

type runConfig struct {
	Input         string
	OutputDir     string
	LibraryDir    string
	Password      string
	TempDir       string
	Workers       int
	Compress      bool
	DeleteSources bool
	NoURI         bool
	NoTUI         bool
	Debug         bool
	NoUpdateCheck bool
	UpdateChecker *selfupdate.Checker
	Started       time.Time
}

func main() {
	console.EnableVT()
	started := time.Now()

	flag.Usage = func() { printHelp(filepath.Base(os.Args[0]), os.Stderr) }

	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastLog %s\n", version.String)
		return
	}
	if cliargs.IsHelpRequest(os.Args[1:]) {
		printHelp(filepath.Base(os.Args[0]), os.Stdout)
		os.Exit(0)
	}
	if handled, err := selfupdate.Dispatch(os.Args[1:], version.String, os.Stdout); handled {
		if err != nil {
			fatalf("%v", err)
		}
		return
	}

	fileCfg, err := config.LoadFromArgv(os.Args[1:])
	if err != nil {
		fatalf("%v", err)
	}

	out := flag.String("o", "", "output directory")
	outDedup := flag.String("od", "", "sfu library directory")
	password := flag.String("p", "", "archive password or password-list file")
	workers := flag.Int("workers", 0, "parser/archive worker count (0=auto)")
	tempDir := flag.String("temp-dir", "", "directory for temp files")
	noTUI := flag.Bool("no-tui", false, "disable live TUI")
	zst := flag.Bool("zst", false, "compress classic output with zstd")
	delSrc := flag.Bool("del", false, "delete source files after success")
	noURI := flag.Bool("no-uri", false, "emit host:login:password")
	debug := flag.Bool("debug", false, "write structured debug log")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable background update check")

	flagArgs, positional := cliargs.SplitPositional(config.StripConfigArgv(os.Args[1:]), flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	visited := config.NewVisited()
	if err := fileCfg.ApplySFL(visited, config.SFLFlags{
		O: out, OD: outDedup, TempDir: tempDir, Password: password,
		Workers: workers,
		NoTUI:   noTUI, Zst: zst, Del: delSrc, NoURI: noURI,
		Debug: debug, NoUpdateCheck: noUpdateCheck,
	}); err != nil {
		fatalf("%v", err)
	}

	inputArg := resolveInputArg(fileCfg, positional)
	if strings.TrimSpace(inputArg) == "" {
		fmt.Fprintln(os.Stderr, "sfl: no input path provided; set [sfl].input in your config or pass INPUT_PATH on the CLI")
		fmt.Fprintln(os.Stderr)
		flag.Usage()
		os.Exit(2)
	}
	if *out != "" && *outDedup != "" {
		usagef("-od and -o are mutually exclusive; pick one")
	}
	destDedup := *outDedup != ""
	if !destDedup && *out == "" {
		*out = "."
	}
	if destDedup {
		*zst = true
	}
	w := resolveWorkerCount(*workers, runtime.GOMAXPROCS(0))

	cfg := runConfig{
		Input: inputArg, OutputDir: *out, LibraryDir: *outDedup, Password: *password,
		TempDir: *tempDir, Workers: w, Compress: *zst, DeleteSources: *delSrc,
		NoURI: *noURI, NoTUI: *noTUI, Debug: *debug, NoUpdateCheck: *noUpdateCheck,
		Started: started,
	}
	cfg.UpdateChecker = selfupdate.NewChecker(version.String, os.Args[0], cfg.NoUpdateCheck)
	cfg.UpdateChecker.Start()
	if err := run(cfg); err != nil {
		fatalf("%v", err)
	}
}

// resolveWorkerCount picks the parser/archive worker count: an explicit
// positive flag wins, otherwise it scales with the available cores (no hard
// cap, so stronger machines parse more archives at once). cpu is GOMAXPROCS.
func resolveWorkerCount(flag, cpu int) int {
	if flag > 0 {
		return flag
	}
	if cpu < 1 {
		return 1
	}
	return cpu
}

func resolveInputArg(fileCfg config.File, positional []string) string {
	switch len(positional) {
	case 0:
		cfgInput, err := fileCfg.ResolvedSFLDir("input")
		if err != nil {
			fatalf("%v", err)
		}
		return cfgInput
	case 1:
		return positional[0]
	default:
		fmt.Fprintf(os.Stderr, "sfl: expected exactly one input path; got %d\n\n", len(positional))
		flag.Usage()
		os.Exit(2)
		return ""
	}
}

// run drives a full extraction: it sets up signal handling, a live progress
// monitor, streams credentials through the shared Engine into the selected sink
// (classic file or, for -od, a temp ULP), then for -od merges that ULP into the
// library in-process via ulpengine so one icy frame spans scan→extract→ingest.
// It optionally deletes parsed sources and prints a single summary. The monitor
// is always torn down before any further stderr output so frames never
// interleave.
func run(cfg runConfig) error {
	// Keep a redirected summary/log free of ANSI; the live frame and summary
	// both target stderr, so color follows stderr's TTY status.
	applyStderrColorProfile()

	ctx, cancel, signaled := signalContext()
	defer cancel()

	// Track open file handles so a graceful Ctrl-C can unstick reads blocked on
	// slow storage; watchInterrupt force-exits if cleanup overruns the grace.
	files := &fileabort.Registry{}
	ctx = fileabort.WithContext(ctx, files)
	go watchInterrupt(ctx, files, signaled)

	dbg := newDebugLogger(cfg)
	defer dbg.Close()

	passwords, err := sflog.LoadPasswords(cfg.Password)
	if err != nil {
		return err
	}

	snk, err := openSink(cfg)
	if err != nil {
		return err
	}
	dbg.Header(cfg, len(passwords), snk.outPath)

	prog := sflog.NewProgress()
	tuiOff := cfg.NoTUI || !stderrIsTTY()
	monDone := make(chan struct{})
	var monWG sync.WaitGroup
	if !tuiOff {
		monWG.Add(1)
		go monitor(monDone, startedOrNow(cfg), prog, signaled, &monWG)
	}
	monitorStopped := false
	stopMonitor := func() {
		if monitorStopped {
			return
		}
		monitorStopped = true
		close(monDone)
		if !tuiOff {
			monWG.Wait()
		}
	}
	defer stopMonitor()

	// One per-run dir for nested-archive spills (honors -temp-dir). Registered so
	// a force-exit before the deferred RemoveAll still surfaces it in the manual
	// cleanup hint; a normal run removes it.
	spillDir, err := os.MkdirTemp(cfg.TempDir, "sfl-spill-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	ulpengine.RegisterCleanupPath(spillDir)
	// removeSpill is idempotent (os.RemoveAll no-ops on a missing path), so the
	// graceful interrupt paths below can clean up explicitly before exitWithCode
	// — which os.Exits and therefore skips this defer — without any double-free.
	removeSpill := func() { _ = os.RemoveAll(spillDir) }
	defer removeSpill()

	eng := buildEngine(cfg, passwords, prog, dbg, spillDir)
	stats, results, extractErr := eng.Run(ctx, cfg.Input, snk.w)
	dbg.Completion(stats)
	if extractErr != nil {
		dbg.Event("extract ended early: %v (signaled=%v)", extractErr, signaled())
	}
	finalizeErr := snk.finalize(extractErr != nil)

	// Extraction/finalize failures tear the live frame down before any stderr so
	// frames never interleave with the error.
	if extractErr != nil {
		stopMonitor()
		snk.cleanup()
		if signaled() {
			removeSpill()
			printInterruptSummary(cfg)
			exitWithCode(130)
		}
		return extractErr
	}
	if finalizeErr != nil {
		stopMonitor()
		snk.cleanup()
		return finalizeErr
	}

	outPath := snk.outPath
	var (
		ingestRes *ulpengine.Resolved
		ingestMet *ulpengine.Metrics
		libEmpty  bool // -od ran but extraction produced nothing to ingest
	)
	if cfg.LibraryDir != "" {
		if stats.Emitted == 0 {
			// Nothing to merge: leave the library untouched and report it as a
			// calm completion (with the issue breakdown), not an error exit.
			libEmpty = true
			stopMonitor()
			dbg.Event("ingest: skipped (nothing emitted)")
		} else {
			// The same icy frame carries through ingest: the monitor stays up
			// while the dedup engine runs in-process, then we tear it down for
			// the summary.
			dbg.Event("ingest: start lib=%q ulp=%q emitted=%d", cfg.LibraryDir, snk.ulpPath, stats.Emitted)
			ingestRes, ingestMet, err = ingestToLibrary(ctx, cfg, snk.ulpPath, prog)
			stopMonitor()
			if err != nil {
				dbg.Event("ingest: ended early: %v (signaled=%v)", err, signaled())
				snk.cleanup()
				if signaled() {
					removeSpill()
					printInterruptSummary(cfg)
					exitWithCode(130)
				}
				return err
			}
			dbg.Event("ingest: done")
		}
	} else {
		stopMonitor()
		if stats.Emitted == 0 {
			// L5: never leave an empty output file behind.
			_ = os.Remove(snk.outPath)
			outPath = "(no ULP detected)"
		}
	}
	snk.cleanup()

	if cfg.DeleteSources {
		deleted, err := deleteParsedSources(cfg.Input, results, snk.protected)
		if err != nil {
			return fmt.Errorf("delete sources: %w", err)
		}
		dbg.Event("del: removed %d source unit(s)", len(deleted))
	}

	// One cohesive summary: classic (-o) reports the output path; -od reports the
	// resulting library size from the in-process ingest, or a "library unchanged"
	// note when nothing was extracted.
	var summary []string
	var updateNotice *selfupdate.Notice
	if cfg.UpdateChecker != nil {
		updateNotice = cfg.UpdateChecker.NoticeForSummary()
	}
	switch {
	case cfg.LibraryDir != "" && libEmpty:
		summary = renderNoIngestSummaryWithNotice(cfg.LibraryDir, stats, updateNotice)
	case cfg.LibraryDir != "":
		var newToLib, alreadyInLib int64
		if ingestMet != nil {
			newToLib = ingestMet.LinesUnique.Load()
			alreadyInLib = ingestMet.LinesSkippedByDest.Load()
		}
		summary = renderIngestSummaryWithNotice(cfg.LibraryDir, ingestLibraryLines(ingestRes, ingestMet), newToLib, alreadyInLib, stats, updateNotice)
	default:
		summary = renderFinalSummaryWithNotice(outPath, stats, updateNotice)
	}
	for _, ln := range summary {
		fmt.Fprintln(os.Stderr, ln)
	}
	return nil
}

// sink abstracts the credential destination so run() handles classic and -od
// modes uniformly. For classic the writer is the (optionally zstd) output file;
// for -od it is a temp ULP file later fed to sfu.
type sink struct {
	w         io.Writer
	file      *os.File
	enc       *zstd.Encoder
	outPath   string
	ulpPath   string
	workDir   string
	protected []string
}

func openSink(cfg runConfig) (*sink, error) {
	if cfg.LibraryDir != "" {
		parent := cfg.TempDir
		if parent == "" {
			libAbs, err := absClean(cfg.LibraryDir)
			if err != nil {
				return nil, err
			}
			parent = filepath.Dir(libAbs)
		}
		// 0700: decrypted credentials must not land in a world-readable dir.
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, err
		}
		workDir, err := os.MkdirTemp(parent, "sfl-od-*")
		if err != nil {
			return nil, err
		}
		// Holds the decrypted ULP; if a force-exit skips snk.cleanup the hint
		// must point the analyst at it for manual removal.
		ulpengine.RegisterCleanupPath(workDir)
		ulpPath := filepath.Join(workDir, "sfl_generated_ulp.txt")
		f, err := os.Create(ulpPath)
		if err != nil {
			_ = os.RemoveAll(workDir)
			return nil, err
		}
		libAbs, _ := absClean(cfg.LibraryDir)
		return &sink{w: f, file: f, outPath: ulpPath, ulpPath: ulpPath, workDir: workDir,
			protected: []string{workDir, libAbs}}, nil
	}

	outPath, err := createOutputPath(cfg)
	if err != nil {
		return nil, err
	}
	f, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	s := &sink{w: f, file: f, outPath: outPath}
	if outDir, err := absClean(cfg.OutputDir); err == nil {
		s.protected = []string{outPath, outDir}
	} else {
		s.protected = []string{outPath}
	}
	if cfg.Compress {
		enc, err := zstd.NewWriter(f)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(outPath)
			return nil, err
		}
		s.enc = enc
		s.w = enc
	}
	return s, nil
}

// finalize closes the encoder and file. failed=true discards a classic output.
func (s *sink) finalize(failed bool) error {
	var err error
	if s.enc != nil {
		if cerr := s.enc.Close(); cerr != nil {
			err = cerr
		}
	}
	if s.file != nil {
		if cerr := s.file.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}
	if failed && s.workDir == "" {
		_ = os.Remove(s.outPath)
	}
	return err
}

func (s *sink) cleanup() {
	if s.workDir != "" {
		_ = os.RemoveAll(s.workDir)
	}
}

func buildEngine(cfg runConfig, passwords []string, prog *sflog.Progress, dbg *debugLogger, spillDir string) *sflog.Engine {
	eng := &sflog.Engine{
		Workers:          cfg.Workers,
		NoURI:            cfg.NoURI,
		Passwords:        passwords,
		Progress:         prog,
		TempDir:          spillDir,
		FollowedByIngest: cfg.LibraryDir != "",
	}
	if dbg != nil {
		eng.Debug = dbg.Event
	}
	return eng
}

// ingestToLibrary merges the generated ULP into the library in-process via the
// shared dedup engine, identical to `sfu -od <lib> <ulp>`. It installs an ingest
// view on prog so the live frame keeps rendering through the merge, and returns
// the resolved run + metrics so the caller can report the final library size.
func ingestToLibrary(ctx context.Context, cfg runConfig, ulpPath string, prog *sflog.Progress) (*ulpengine.Resolved, *ulpengine.Metrics, error) {
	ulpBytes := fileSizeOrZero(ulpPath)
	m := &ulpengine.Metrics{}
	var od atomic.Pointer[ulpengine.ODMetrics]

	// lastFrac clamps the bar monotonically. The closure is polled only by the
	// monitor goroutine, so a plain captured float needs no synchronization.
	var lastFrac float64
	prog.BeginIngest(func() sflog.IngestView {
		v := ingestView(m, od.Load(), ulpBytes)
		v.Fraction = monotonic(v.Fraction, &lastFrac)
		return v
	})

	// Capture the engine's ingest events (shard/dedup phases, -od scan/regen) to
	// a sibling of sfl's own debug log when -debug is set. nil DebugLog is a no-op
	// in the engine, so the unconditional pass-through is safe.
	elog := newIngestDebugLog(cfg)
	defer elog.Close()

	opts := ulpengine.IngestOptions{
		ULPPath:    ulpPath,
		LibraryDir: cfg.LibraryDir,
		Workers:    cfg.Workers,
		TempDir:    cfg.TempDir,
		NoURI:      cfg.NoURI,
		RunStarted: startedOrNow(cfg),
		Debug:      elog,
		OnResolved: func(r *ulpengine.Resolved) {
			if r != nil {
				od.Store(r.OdMetrics)
			}
		},
	}
	r, err := ulpengine.Ingest(ctx, opts, m)
	return r, m, err
}

// monotonic returns the larger of cur and *last, updating *last to that value.
// It guards the displayed ingest bar against the engine's concurrent phase
// reorder so the fraction can never visibly reverse.
func monotonic(cur float64, last *float64) float64 {
	if cur < *last {
		return *last
	}
	*last = cur
	return cur
}

// newIngestDebugLog opens an engine debug log for the in-process ingest next to
// sfl's own debug log (library dir for -od), or returns nil when -debug is off.
// A nil *ulpengine.DebugLog is safe to pass and Close.
func newIngestDebugLog(cfg runConfig) *ulpengine.DebugLog {
	if !cfg.Debug {
		return nil
	}
	dir := cfg.LibraryDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	path := filepath.Join(dir, "sfl_ingest_debug_"+startedOrNow(cfg).Format("20060102_150405")+".log")
	elog, err := ulpengine.NewDebugLog(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sfl: ingest debug log disabled: %v\n", err)
		return nil
	}
	return elog
}

// ingestView snapshots the dedup engine's atomics into the icy INGESTING frame:
// an overall fraction blended across the engine phases plus added/already-there
// counts.
func ingestView(m *ulpengine.Metrics, od *ulpengine.ODMetrics, ulpBytes int64) sflog.IngestView {
	frac, status := ingestProgress(m, od, ulpBytes)
	return sflog.IngestView{
		Fraction: frac,
		Unique:   m.LinesUnique.Load(),
		Skipped:  m.LinesSkippedByDest.Load(),
		Status:   status,
	}
}

// ingestProgress maps the engine's phase + byte counters onto a 0..1 bar.
//
// The -od pipeline is concurrent and re-orders phases: shard (reading the small
// ULP) runs alongside phase-0 regen, and m.Phase flips BACK to phasePhase0 after
// shard while regen drains (see internal/ulpengine/pipeline.go). Keying purely on
// m.Phase would shoot the bar to ~70% on the fast shard, then snap it back to ~5%
// for the slow regen. So the pre-dedup region [0.03, 0.65] is driven by the
// dominant cold-library cost (regen) when present, else by the ULP read, both of
// which advance monotonically. Dedup owns [0.65, 0.97], index 0.97, done 1.0.
// The caller's BeginIngest closure also clamps the result monotonically as a
// belt-and-suspenders against any residual reorder.
func ingestProgress(m *ulpengine.Metrics, od *ulpengine.ODMetrics, ulpBytes int64) (float64, string) {
	switch m.Phase.Load() {
	case ulpengine.PhaseDone:
		return 1.0, "done"
	case ulpengine.PhaseIndex:
		return 0.97, "finalizing library index…"
	case ulpengine.PhaseDedup:
		frac := 0.70
		if tot := m.BucketsBytesTotal.Load(); tot > 0 {
			frac = 0.65 + 0.32*clampFrac(float64(m.BucketsBytesRead.Load())/float64(tot))
		}
		return frac, "merging & deduplicating against library…"
	default:
		// PhaseInit / PhasePhase0 / PhaseShard: the concurrent pre-dedup region.
		// Regen is the long pole on a cold library and runs concurrently with the
		// quick shard, so prefer it; ignore the shard's fast climb that would
		// otherwise invert the bar when m.Phase returns to phasePhase0.
		if od != nil {
			if tot := od.RegenBytesTotal.Load(); tot > 0 {
				return 0.03 + 0.62*clampFrac(float64(od.RegenBytesRead.Load())/float64(tot)),
					"rebuilding library index…"
			}
		}
		if ulpBytes > 0 {
			return 0.03 + 0.62*clampFrac(float64(m.BytesRead.Load())/float64(ulpBytes)),
				"reading extracted credentials…"
		}
		return 0.03, "scanning library…"
	}
}

func clampFrac(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// ingestLibraryLines is the indexed line count across the whole library after
// the ingest: prior archives (loaded in phase 0) plus the unique lines just
// written. Mirrors sfu's libraryLineCountTotal.
func ingestLibraryLines(r *ulpengine.Resolved, m *ulpengine.Metrics) int64 {
	if r == nil || r.OdResult == nil {
		if m != nil {
			return m.LinesUnique.Load()
		}
		return 0
	}
	total := int64(r.OdResult.TotalKeysLoaded)
	if m != nil {
		total += m.LinesUnique.Load()
	}
	return total
}

func fileSizeOrZero(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

func startedOrNow(cfg runConfig) time.Time {
	if cfg.Started.IsZero() {
		return time.Now()
	}
	return cfg.Started
}

func printInterruptSummary(cfg runConfig) {
	for _, ln := range renderInterruptSummary(time.Since(startedOrNow(cfg))) {
		fmt.Fprintln(os.Stderr, ln)
	}
}

func createOutputPath(cfg runConfig) (string, error) {
	if cfg.OutputDir == "" {
		cfg.OutputDir = "."
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return "", err
	}
	started := cfg.Started
	if started.IsZero() {
		started = time.Now()
	}
	name := "sfl_" + started.Format("20060102_150405") + ".txt"
	if cfg.Compress {
		name += ".zst"
	}
	return filepath.Join(cfg.OutputDir, name), nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfl: "+format+"\n", args...)
	exitWithCode(1)
}

func usagef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfl: "+format+"\n\n", args...)
	flag.Usage()
	exitWithCode(2)
}
