package main

import (
	"context"
	"errors"
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
	"github.com/snowx-dev/SnowFastULP/internal/bell"
	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/console"
	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/outdir"
	"github.com/snowx-dev/SnowFastULP/internal/pathident"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// reg is the shared terminal restore/exit registry: the live screen registers
// its teardown via Set, and every exit path routes through it so the
// alt-screen is always left cleanly. ulpengine.PrintManualCleanupHint prints
// stranded scratch paths on a force-exit (second Ctrl-C / cleanup timeout).
var reg = termctl.New(os.Stderr, ulpengine.PrintManualCleanupHint)

// Test seams for the signal-shutdown path: only a real SIGINT/SIGTERM can set
// the registry's signal flag in production, so tests inject a deterministic
// mid-tail signal by flipping signalForce from a hook that runs inside sink
// finalization (finalizeHook) or the history commit (the syncHistoryOutputs
// override), and capture the interrupt exit code through exitInterrupted
// instead of letting os.Exit kill the test process. Production never sets
// signalForce or finalizeHook; exitInterrupted keeps the real ExitWithCode.
var (
	signalForce     func() bool
	finalizeHook    func()
	exitInterrupted = func() { reg.ExitWithCode(termctl.InterruptExitCode()) }
)

// runOutcome reports a completed run whose exit code is nonzero per the
// shared policy (internal/exitcode): extraction finished and the summary box
// was printed, but the outcome is not clean (some or all sources failed, or
// nothing was discovered). main maps it to the process exit code; the message
// rides the -json error terminal since the human summary is already on
// stderr. The all-failed arm only means 4 when nothing was written
// (stats.Emitted == 0): a salvaged truncated set that streamed usable lines
// to the output is partial (3), never a contradiction of the file on disk.
type runOutcome struct {
	code int
	msg  string
}

func (e *runOutcome) Error() string { return e.msg }

// restoreOnPanic is the terminal panic guard: on panic, restore the registered
// terminal hook (leave the alt screen, show the cursor), then re-panic so the
// crash still surfaces with its stack trace. main installs it as the outer
// defer — after run()'s own JSON error-terminal recovery, which stays inside
// run — and the TUI goroutine wrapper uses it so a render/teardown panic
// restores before the goroutine dies. Never swallows the panic.
func restoreOnPanic() {
	if r := recover(); r != nil {
		reg.Restore()
		panic(r)
	}
}

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
	Loose         bool
	NoTUI         bool
	Debug         bool
	DebugReject   bool
	// RunStamp is the one shared run name ("YYYYMMDD_<six-char-id>") named
	// after by the classic ULP output, the -env dir, and the -od
	// ingest archive. Empty until run() calls ensureRunStamp; tests inject it.
	RunStamp      string
	NoUpdateCheck bool
	Env           bool
	UpdateChecker *selfupdate.Checker
	Started       time.Time
	// DryRun (-odr): run the full extract+ingest pipeline but write nothing
	// to the library; the summary reports what would have been added.
	DryRun      bool
	History     bool
	HistoryPath string
	// JSONOut ("-json"): "" = off, "-" = stdout, else an NDJSON file
	// path. JSONOutLiteral distinguishes the explicit file:- escape from stdout.
	JSONOut        string
	JSONOutLiteral bool
	JSONEvery      time.Duration
	// ForcePlainTUI is set when VT processing can't be enabled (legacy Windows
	// console), forcing plain output so ANSI escapes never leak as raw text.
	ForcePlainTUI bool
	Bell          bool
}

func main() {
	// A panic anywhere below (e.g. in run while the monitor goroutine owns the
	// alt-screen) would otherwise crash with the screen still alternate and
	// the cursor hidden. Restore first, then re-panic so the stack trace
	// prints on a clean screen. No-op until the monitor installs the hook.
	defer restoreOnPanic()
	// vtOK is false only on a legacy Windows console that can't render ANSI;
	// it flows into runConfig to force plain mode so escapes never leak.
	vtOK := console.EnableVT()
	started := time.Now()

	// flag.Usage is the trivial-error footer: a single hint line, not the
	// full help dump — the full help stays on explicit -h (printHelp). The
	// flag package also calls it after a parse error (ExitOnError), so
	// undefined or malformed flags stay short too.
	flag.Usage = func() { fmt.Fprintln(os.Stderr, "run with -h for help") }

	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastLog %s\n", version.String)
		return
	}
	// `update` / `upgrade`: replace installed SnowFast binaries with the latest release.
	// Handled before --help so `sfl update --help` reaches the update subcommand's
	// own help text instead of the generic top-level help.
	if handled, err := selfupdate.Dispatch(os.Args[1:], version.String, os.Stdout); handled {
		if err != nil {
			fatalf("%v", err)
		}
		return
	}
	if cliargs.IsHelpRequest(os.Args[1:]) {
		printHelp(filepath.Base(os.Args[0]), os.Stdout)
		reg.ExitWithCode(0)
	}

	config.EnsureMigrated(os.Args[1:], version.String, os.Stderr)
	fileCfg, err := config.LoadFromArgv(os.Args[1:])
	if err != nil {
		fatalf("%v", err)
	}

	out := flag.String("o", "", "output directory")
	outDedup := flag.String("od", "", "sfu library directory")
	outDryRun := flag.String("odr", "", "like -od but writes nothing: preview what a run would add to the library")
	password := flag.String("p", "", "archive password or password-list file")
	workers := flag.Int("workers", 0, "parser/archive worker count (0=auto)")
	workersAlias := flag.Int("j", 0, "alias for -workers")
	tempDir := flag.String("temp-dir", "", "directory for temp files")
	noTUI := flag.Bool("no-tui", false, "disable live TUI")
	zst := flag.Bool("zst", false, "compress classic output with zstd")
	delSrc := flag.Bool("del", false, "delete source files after success")
	noURI := flag.Bool("no-uri", false, "emit host:login:password")
	loose := flag.Bool("loose", false, "high-recall parser: also accept bare host/IP forms and other less precise shapes; merges creds that differ only by URL path")
	debug := flag.Bool("debug", false, "write structured debug log")
	debugReject := flag.Bool("debug-reject", false, "append library-ingest rejected lines to a file next to the ingest debug log")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable background update check")
	envOn := flag.Bool("env", false, "copy env/key files flat into <out>/sfl_<date>_<id>_secrets/")
	historyOn := flag.Bool("history", false, "skip sources already completed in the shared history database")
	historyPath := flag.String("history-path", "", "shared history SQLite database path (does not enable history)")
	jsonOutFlag := &cliargs.OutTarget{}
	flag.Var(jsonOutFlag, "json", "stream live stats as one JSON object per line: bare = stdout, =FILE = file")
	jsonEvery := flag.Duration("json-every", defaultJSONEvery, "update interval for -json")
	bellOn := flag.Bool("bell", false, "play a short sound when the run finishes")

	argv := config.StripConfigArgv(os.Args[1:])
	for i, arg := range argv {
		if arg == "--" {
			break
		}
		if arg == "-json" && i+1 < len(argv) && argv[i+1] == "-" {
			usagef("-json - is ambiguous; use bare -json or -json=- for stdout, or -json=file:- for a literal file named -")
		}
	}
	flagArgs, positional := cliargs.SplitPositional(argv, flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		reg.ExitWithCode(2)
	}
	visited := config.NewVisited()
	if err := fileCfg.ApplyHistory(visited, config.HistoryFlags{Enabled: historyOn, Path: historyPath}); err != nil {
		fatalf("%v", err)
	}
	if visited["history-path"] && strings.TrimSpace(*historyPath) == "" {
		usagef("-history-path requires a file or directory path; got empty string")
	}
	if *historyOn && *historyPath == "" {
		resolved, err := config.DefaultHistoryPath()
		if err != nil {
			fatalf("history path: %v", err)
		}
		*historyPath = resolved
	}
	// Accept -j as an alias for -workers (sfs uses -j) so the same invocation
	// works across all three CLIs; explicit -workers wins.
	visited.ResolveIntAlias(workers, workersAlias, "workers", "j")
	var odrCfg bool
	if err := fileCfg.ApplySFL(visited, config.SFLFlags{
		O: out, OD: outDedup, ODR: &odrCfg, TempDir: tempDir, Password: password,
		Workers: workers,
		NoTUI:   noTUI, Zst: zst, Del: delSrc, NoURI: noURI,
		Debug: debug, DebugReject: debugReject, NoUpdateCheck: noUpdateCheck,
		Bell:    bellOn,
		Loose:   loose,
		Env:     envOn,
		JSONOut: jsonOutFlag, JSONEvery: jsonEvery,
	}); err != nil {
		fatalf("%v", err)
	}

	inputArg := resolveInputArg(fileCfg, positional)
	if strings.TrimSpace(inputArg) == "" {
		fmt.Fprintln(os.Stderr, "sfl: no input path provided; set [sfl].input in your config or pass INPUT_PATH on the CLI")
		flag.Usage()
		reg.ExitWithCode(2)
	}
	// -o, -od, -odr mutually exclusive. -odr is -od's dry-run twin: same
	// pipeline + stats, no library writes. Config [sfl] odr=true flips
	// dry-run on a -od run, reusing the od path.
	odPassed := visited["od"]
	odrPassed := visited["odr"]
	outCount := 0
	if *out != "" {
		outCount++
	}
	if *outDedup != "" {
		outCount++
	}
	if *outDryRun != "" {
		outCount++
	}
	if outCount > 1 {
		usagef("-o, -od, and -odr are mutually exclusive; pick one")
	}
	if odPassed && strings.TrimSpace(*outDedup) == "" {
		usagef("-od requires a directory path; got empty string")
	}
	if odrPassed && strings.TrimSpace(*outDryRun) == "" {
		usagef("-odr requires a directory path; got empty string")
	}
	dryRun := false
	destDedup := *outDedup != "" || *outDryRun != ""
	if !destDedup && *out == "" {
		*out = "."
	}
	if destDedup {
		*zst = true
	}
	if *outDryRun != "" {
		dryRun = true
	}
	if !dryRun && odrCfg {
		if !destDedup {
			usagef("[sfl] odr=true requires a library path; set [sfl].od or pass -od/-odr DIR")
		}
		dryRun = true
	}
	w := resolveWorkerCount(*workers, runtime.GOMAXPROCS(0))

	libraryDir := *outDedup
	if *outDryRun != "" {
		libraryDir = *outDryRun
	}
	// Output dir preflight validates both dry-run and real runs without creating
	// missing dirs; sink setup creates them after all validation completes.
	if destDedup {
		libFlag := "-od"
		if *outDryRun != "" {
			libFlag = "-odr"
		}
		if err := preflightOutputDir(libFlag, libraryDir); err != nil {
			usagef("%v", err)
		}
	} else {
		if err := preflightOutputDir("-o", *out); err != nil {
			usagef("%v", err)
		}
	}
	cfg := runConfig{
		Input: inputArg, OutputDir: *out, LibraryDir: libraryDir, Password: *password,
		TempDir: *tempDir, Workers: w, Compress: *zst, DeleteSources: *delSrc,
		NoURI: *noURI, Loose: *loose, NoTUI: *noTUI, Debug: *debug, DebugReject: *debugReject, NoUpdateCheck: *noUpdateCheck,
		Env:     *envOn,
		Started: started, ForcePlainTUI: !vtOK,
		DryRun:  dryRun,
		History: *historyOn, HistoryPath: *historyPath,
		JSONOut: jsonOutTarget(jsonOutFlag), JSONOutLiteral: jsonOutFlag.Literal, JSONEvery: *jsonEvery,
		Bell: *bellOn,
	}
	cfg.UpdateChecker = selfupdate.NewChecker(version.String, os.Args[0], cfg.NoUpdateCheck)
	cfg.UpdateChecker.Start()
	if err := run(cfg); err != nil {
		var targetErr *jsonOutTargetError
		if errors.As(err, &targetErr) {
			usagef("%v", err)
		}
		var uerr *usageError
		if errors.As(err, &uerr) {
			usagef("%v", err)
		}
		var outcome *runOutcome
		if errors.As(err, &outcome) {
			reg.ExitWithCode(outcome.code)
		}
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
		fmt.Fprintf(os.Stderr, "sfl: expected exactly one input path; got %d\n", len(positional))
		flag.Usage()
		reg.ExitWithCode(2)
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
func run(cfg runConfig) (runErr error) {
	// Keep a redirected summary/log free of ANSI; the live frame and summary
	// both target stderr, so color follows stderr's TTY status.
	applyStderrColorProfile()

	// One shared run stamp before anything is opened or swept: the classic ULP
	// output, the -env dir, and the -od ingest archive must all name
	// themselves after the same date+id with no time-of-day.
	if err := ensureRunStamp(&cfg); err != nil {
		return err
	}
	if err := validateJSONOutTarget(cfg); err != nil {
		return err
	}
	// The stream interval is rejected here, before any sink is opened, so a
	// usage error leaves neither an empty classic output nor -od staging
	// behind. newJSONOut keeps its own guard for the emitter's invariant.
	if cfg.JSONOut != "" && cfg.JSONEvery <= 0 {
		// M-11: a non-positive interval is argv-shape validation, not a
		// runtime failure — usage exit code, like sfu/sfs.
		return &usageError{fmt.Errorf("invalid -json-every %v: must be positive", cfg.JSONEvery)}
	}

	sweepOrphanWorkDirs(cfg)
	ctx, cancel, sig := reg.SignalContext()
	defer cancel()
	// signalForce is a test seam consulted alongside the real signal flag:
	// tests raise a deterministic mid-tail signal through it (see seams above).
	signaled := sig
	if signalForce != nil {
		forced := signalForce
		signaled = func() bool { return sig() || forced() }
	}

	// Track open file handles so a graceful Ctrl-C can unstick reads blocked on
	// slow storage; WatchInterrupt force-exits if cleanup overruns the grace.
	files := &fileabort.Registry{}
	ctx = fileabort.WithContext(ctx, files)
	go reg.WatchInterrupt(ctx, files, signaled)

	dbg := newDebugLogger(cfg)
	defer dbg.Close()

	iss := newIssueLogger(cfg)
	defer iss.Close()

	passwords, err := sflog.LoadPasswords(cfg.Password)
	if err != nil {
		return err
	}
	var historyStore history.Store
	if cfg.History {
		historyStore, err = openHistoryStore(cfg.HistoryPath, cfg.DryRun)
		if err != nil {
			return fmt.Errorf("open history: %w", err)
		}
		if historyStore != nil {
			defer historyStore.Close()
		}
	}

	snk, err := openSink(cfg)
	if err != nil {
		return err
	}
	// Arm the sink discard immediately after opening: a failure between here
	// and normal finalization (e.g. an unopenable -json target) must leave
	// neither an empty classic output nor -od staging behind. Disarmed once
	// finalize commits the output below; abort is idempotent, so failure paths
	// that tear the sink down explicitly stay safe.
	snkArmed := true
	// interruptedDone records whether the signal-shutdown path
	// (finishInterrupted, below) already emitted the stream's interrupted
	// terminal, so the deferred stop can never classify the run behind it.
	interruptedDone := false
	defer func() {
		if snkArmed {
			snk.abort()
		}
	}()
	dbg.Header(cfg, len(passwords), snk.outPath)

	prog := sflog.NewProgress()
	prog.SetDryRun(cfg.DryRun)
	if cfg.LibraryDir != "" {
		prog.SetLibrary(true)
	}
	jout, err := newJSONOut(cfg, prog, reg)
	if err != nil {
		return err
	}
	jout.start()
	// The deferred stop closes the stream on normal returns — with the run's
	// real outcome, since non-signaled failures must end the stream in error,
	// not done. The exit paths below that call reg.ExitWithCode (os.Exit,
	// which skips defers) stop the stream explicitly first.
	defer func() {
		// A panic must not end the stream in "done": recover, emit the error
		// terminal with the panic value, then re-panic so the crash still
		// surfaces with its stack trace.
		if r := recover(); r != nil {
			jout.stop(tuistat.EventError, fmt.Sprintf("panic: %v", r))
			panic(r)
		}
		// A signal shutdown owns the terminal: finishInterrupted already
		// emitted the interrupted event, and no done/error may follow it. A
		// signal observed without reaching a shutdown site (e.g. during
		// setup) still ends the stream interrupted rather than done/error.
		if interruptedDone {
			return
		}
		if signaled() {
			jout.stop(tuistat.EventInterrupted, "")
			return
		}
		if runErr != nil {
			// Terminal error events carry the exit code the process will end
			// with (internal/exitcode): 3/4 for classified outcomes, 2 for
			// usage-level target errors, 1 for everything that would fatal.
			var oc *runOutcome
			if errors.As(runErr, &oc) {
				jout.stopWithCode(tuistat.EventError, runErr.Error(), oc.code)
				return
			}
			var targetErr *jsonOutTargetError
			if errors.As(runErr, &targetErr) {
				jout.stopWithCode(tuistat.EventError, runErr.Error(), exitcode.Usage)
				return
			}
			jout.stopWithCode(tuistat.EventError, runErr.Error(), exitcode.Error)
			return
		}
		jout.stop(tuistat.EventDone, "")
	}()
	// -no-tui hides the live screen only. -json hides it only when the
	// stream target is stdout and stdout is a terminal. The end summary still
	// prints on stderr. The config merge has already run, so this covers
	// json_out in the file too.
	tuiOff := !tuistat.ShowLiveDisplay(
		cfg.NoTUI,
		cfg.JSONOut,
		stderrIsTTY(),
		stdoutIsCharDevice(),
		!cfg.ForcePlainTUI,
	)
	monDone := make(chan struct{})
	var monWG sync.WaitGroup
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
	// graceful interrupt paths below can clean up explicitly before reg.ExitWithCode
	// — which os.Exits and therefore skips this defer — without any double-free.
	removeSpill := func() {
		ulpengine.RemoveTreeLogged(spillDir)
		// unregister only when removal actually left it absent; a surviving
		// tree stays registered so a force-exit can still clean it up
		if _, err := os.Stat(spillDir); os.IsNotExist(err) {
			ulpengine.UnregisterCleanupPath(spillDir)
		}
	}
	defer removeSpill()

	eng := buildEngine(cfg, passwords, prog, dbg, iss, spillDir)
	eng.History = historyStore

	var envCopier *sflog.EnvCopier
	if cfg.Env && !cfg.DryRun {
		envCopier = sflog.NewEnvCopier(resolveEnvDir(cfg), prog, 0)
		envCopier.SetErrorHandler(func(issue sflog.EnvCopyIssue) {
			if iss == nil {
				return
			}
			iss.Record(issue.Path, sflog.IssueEnvCopy,
				fmt.Errorf("env-copy %s: %w", issue.Kind, issue.Err))
		})
		envCopier.Start()
		prog.EnableEnv()
		eng.EnvCopier = envCopier
	}

	// History check before the monitor: the sampled prehash is sub-second, so
	// it renders on the inline pre-pass bar (like validating) while stderr is
	// still free; the live monitor starts only once extraction is imminent.
	// With -json or -no-tui the check is silent, like every inline surface.
	var checkBar = newSflHistoryBar(func() io.Writer {
		if !tuiOff && historyStore != nil {
			return stderrFile
		}
		return nil
	}(), "checking")
	stopBar := make(chan struct{})
	var barWG sync.WaitGroup
	if checkBar != nil {
		barWG.Add(1)
		go func() {
			defer barWG.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stopBar:
					return
				case <-ticker.C:
					checkBar.Update(0, 0, prog.HistoryBytesDone(), prog.HistoryBytesTotal())
				}
			}
		}()
	}
	prep, err := eng.PrepareHistory(ctx, cfg.Input)
	prog.SetHistorySkipped(prep.Skipped()) // ingest header's muted skip badge
	if checkBar != nil {
		close(stopBar)
		barWG.Wait()
		checkBar.Finish()
	}
	if err != nil {
		return err
	}

	if !tuiOff {
		monWG.Add(1)
		go monitor(monDone, startedOrNow(cfg), prog, signaled, &monWG, cfg.LibraryDir != "")
	}

	stats, results, extractErr := eng.RunPrepared(ctx, snk.w, prep)
	if envCopier != nil {
		es := envCopier.Close()
		markEnvCopyIssues(results, es.Issues)
		stats.EnvCopied = es.Copied
		stats.EnvDeduped = es.Deduped
		stats.EnvSkippedOverCap = es.SkippedOverCap
		stats.EnvWriteErrors = es.WriteErrors
		stats.EnvOpenErrors = es.OpenErrors
		stats.EnvReadErrors = es.ReadErrors
		stats.EnvWriteFailures = es.WriteFailures
		stats.EnvCollisionErrors = es.CollisionErrors
		stats.EnvTdataErrors = es.TdataErrors
		stats.EnvDirsCopied = es.DirsCopied
		stats.EnvDirsSkippedOverCap = es.DirsSkippedOverCap
		dbg.Event("env: copied=%d deduped=%d skipped=%d errors=%d open=%d read=%d write=%d collision=%d tdata=%d dirs=%d tdata-skip=%d",
			es.Copied, es.Deduped, es.SkippedOverCap, es.WriteErrors,
			es.OpenErrors, es.ReadErrors, es.WriteFailures,
			es.CollisionErrors, es.TdataErrors,
			es.DirsCopied, es.DirsSkippedOverCap)
	}
	dbg.Completion(stats)
	dbg.Issues(stats)
	// F3 (#6)/F4 (#7) Input-count honesty: history-skipped sources never reach
	// the extraction counters — an all-skipped rerun would recap "Input 0
	// archives · 0 files" right after discovery announced "N found". The hits
	// in results are exactly the skipped units (only credential files and
	// archives are fingerprinted; env/tdata items are not), so rolling them
	// into the scanned totals keeps the recap and the JSON summary truthful.
	// Nested archives stay folded into ArchivesScanned; the recap splits them
	// per-source (recapCountRows).
	for _, r := range results {
		if !r.HistoryHit {
			continue
		}
		if r.IsArchive {
			stats.ArchivesScanned++
		} else {
			stats.FilesScanned++
		}
	}
	// Exit-code classification (internal/exitcode): a source counts as failed
	// when it errored out (!OK) or finished with any recorded issue (wrong
	// password, parse failure, no credentials, env-copy failure, ...). A
	// history-skipped source is OK and HadIssue=false, so an all-hit -history
	// rerun stays a clean exit 0.
	failedSources := 0
	for _, r := range results {
		if !r.OK || r.HadIssue {
			failedSources++
		}
	}
	noSources := len(results) == 0
	stats.FailedSources = failedSources
	// Extraction and env copying are done: close the automatic issue log now so
	// its result can be summarized. The deferred close below is then idempotent.
	issueRes := iss.Close()
	// Stage the extraction rollup for the final "summary" event: even an
	// ingest failure or interrupt after this point summarizes what was parsed.
	jout.setSummaryStats(stats)

	// finishInterrupted is the single signal-shutdown path for everything
	// after extraction: it stops the monitor, emits exactly one JSON
	// interrupted terminal, cleans the sink and spill/staging safely (a
	// committed output is never discarded, an uncommitted one is), prints the
	// interrupt summary, and exits 130. Idempotent via finishOnce so the
	// repeated signal checks in run's tail can neither emit a second terminal
	// nor clean up twice.
	finishOnce := &sync.Once{}
	finishInterrupted := func() {
		finishOnce.Do(func() {
			interruptedDone = true
			stopMonitor()
			jout.stop(tuistat.EventInterrupted, "")
			if snkArmed {
				// Finalization never committed: discard the partial output
				// like a failure path would.
				snk.abort()
				snkArmed = false
			} else {
				snk.cleanup()
			}
			interruptCleanup()
			printInterruptSummary(cfg)
			reportIssueLog(issueRes)
			exitInterrupted()
		})
	}

	if extractErr != nil {
		dbg.Event("extract ended early: %v (signaled=%v)", extractErr, signaled())
	}
	finalizeErr := snk.finalize(extractErr != nil)
	// Normal finalization complete: the output is committed, so neither the
	// signal-shutdown path nor later failure paths (history, -del) may discard
	// it.
	if finalizeErr == nil {
		snkArmed = false
	}

	// Extraction/finalize failures tear the live frame down before any stderr so
	// frames never interleave with the error.
	if extractErr != nil {
		if signaled() {
			finishInterrupted()
			return nil // unreachable in production: exitInterrupted exits
		}
		stopMonitor()
		// non-signaled failure: tear the sink down like the finalizeErr path,
		// so a -od staging ULP is never left behind
		reportIssueLog(issueRes)
		snk.abort()
		return extractErr
	}
	// A signal that arrived during finalization wins over the outcome: it is
	// never classified as a finalize error or a successful done.
	if signaled() {
		finishInterrupted()
		return nil // unreachable in production: exitInterrupted exits
	}
	if finalizeErr != nil {
		stopMonitor()
		reportIssueLog(issueRes)
		snk.abort()
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
			if err != nil {
				dbg.Event("ingest: ended early: %v (signaled=%v)", err, signaled())
				if signaled() {
					finishInterrupted()
					return nil // unreachable in production: exitInterrupted exits
				}
				// non-signaled failure: the plaintext staging ULP is no longer
				// needed either, so tear the sink down like the extract path
				stopMonitor()
				snk.abort()
				reportIssueLog(issueRes)
				return err
			}
			stopMonitor()
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
	// Stage the output path and the -od ingest outcome for the final
	// "summary" event before any of the exits below.
	jout.setSummaryTail(outPath, ingestMet, ingestRes)
	// A signal that arrived during the ingest (or the classic post-finalize
	// window) wins over everything below.
	if signaled() {
		finishInterrupted()
		return nil // unreachable in production: exitInterrupted exits
	}
	// Parse quality no longer withholds history (product decision 2026-09-30):
	// rejected lines are deterministic, so a recorded source re-extracts
	// identically and -del may delete it per the user's call. Extraction
	// failures (passwords, missing volumes, read errors) are recorded complete
	// the same way — failed sources are NOT retried under -history; only
	// interrupted runs (130/143) record nothing. Post emit-gate, ingest
	// rejects indicate a producer/parser contract bug — log them, but record.
	historyNotRecordedMsg := ""
	historyCommitErr := error(nil)
	if cfg.History && !cfg.DryRun && ingestMet != nil && ingestMet.LinesRejected.Load() != 0 {
		dbg.Event("history: library ingest rejected %d line(s)%s; recording anyway", ingestMet.LinesRejected.Load(), ingestRejectBreakdown(ingestMet))
	}
	historyOutputs := make([]string, 0, 3)
	if cfg.LibraryDir != "" {
		historyOutputs = append(historyOutputs, ingestHistoryOutputPaths(ingestRes)...)
	} else if stats.Emitted > 0 {
		historyOutputs = append(historyOutputs, snk.outPath)
	}
	if stats.EnvCopied > 0 || stats.EnvDirsCopied > 0 {
		historyOutputs = append(historyOutputs, resolveEnvDir(cfg))
	}
	if issueRes.Path != "" {
		historyOutputs = append(historyOutputs, issueRes.Path)
	}
	// The history validating bar draws only when the TUI would draw (the
	// monitor is down by here, so stderr is free); pipes, logs, and
	// JSON-stream runs stay clean.
	var historyProgWrite io.Writer
	if !tuiOff {
		historyProgWrite = stderrFile
	}
	if historyNotRecordedMsg == "" {
		if err := commitHistory(ctx, historyStore, results, cfg.DryRun, historyProgWrite, historyOutputs...); err != nil {
			// A signal during the history commit wins over the error outcome.
			if signaled() {
				finishInterrupted()
				return nil // unreachable in production: exitInterrupted exits
			}
			// Same recoverable state as the ingest-reject guard: the output is
			// committed and the sources are retained, and the failed history
			// write surfaces via exit code 3 plus the diagnostic line below.
			historyNotRecordedMsg = fmt.Sprintf("history: commit failed: %v; output committed, history not recorded and sources retained", err)
			historyCommitErr = err
			dbg.Event("history: commit failed: %v", err)
		}
	}

	var deletedPaths []string
	if cfg.DeleteSources && !cfg.DryRun && historyNotRecordedMsg == "" {
		// Deletion is globally suppressed when history was not recorded: the
		// run's state is incomplete, so the sources must stay untouched.
		// The effective history database and its WAL/SHM sidecars join the
		// protected set: a directory-group deletion recurses over everything
		// under a successful top-level child, so a database seeded inside an
		// input group would be unlinked while the connection is open.
		protected := append([]string(nil), snk.protected...)
		if paths, e := history.EffectiveDatabasePaths(cfg.HistoryPath); e == nil {
			protected = append(protected, paths...)
		}
		deleted, err := deleteParsedSources(cfg.Input, results, protected)
		if err != nil {
			// A signal during source deletion wins over the error outcome.
			if signaled() {
				finishInterrupted()
				return nil // unreachable in production: exitInterrupted exits
			}
			reportIssueLog(issueRes)
			return fmt.Errorf("delete sources: %w", err)
		}
		// -del transparency: the summary must say what was destroyed. Failed
		// sources are kept by design; count them so "Preserved" shows why
		// inputs survived a -del run. Path list is kept for the all-history-
		// skip one-liner path, which has no recap Deleted row (sfu parity).
		deletedPaths = deleted
		stats.DeletedSources = len(deleted)
		preserved := 0
		for _, r := range results {
			if !r.OK || (r.HadIssue && !r.HistoryComplete) {
				preserved++
			}
		}
		stats.PreservedSources = preserved
		dbg.Event("del: removed %d source unit(s), preserved %d failed source(s)", len(deleted), preserved)
	}

	// Last guard before the summary: a signal that arrived while history was
	// recorded or sources were deleted wins over the done classification.
	if signaled() {
		finishInterrupted()
		return nil // unreachable in production: exitInterrupted exits
	}

	// One cohesive summary: classic (-o) reports the output path; -od reports the
	// resulting library size from the in-process ingest, or a "library unchanged"
	// note when nothing was extracted.
	var summary []string
	var updateNotice *selfupdate.Notice
	if cfg.UpdateChecker != nil {
		updateNotice = cfg.UpdateChecker.NoticeForSummary()
	}
	// When history could not be recorded, the rendered summary stays silent:
	// the state is signaled by exit code 3 and the issues log instead of a
	// title suffix or in-box note.
	switch {
	// Pure all-history-skip run: every discovered source was already
	// completed, nothing parsed, emitted, copied, failed or issued — match
	// sfu's history-only summary exactly (cmd/sfu/history.go): one ✓ line to
	// stderr, no recap box, no update banner/tagline. -odr previews keep the
	// box: the dry-run recap carries preview info the one-liner would hide.
	// When -del removed sources, the path list is printed after the ✓ line
	// (same shape as sfu's history-only -del report); the recap Deleted row
	// is unavailable on this path.
	case !cfg.DryRun && historyCommitErr == nil && historyAllSkipped(stats):
		summary = []string{historySkipSummaryLine(stats.HistorySkipped)}
	case cfg.LibraryDir != "" && libEmpty:
		summary = renderNoIngestSummaryWithNotice(cfg.LibraryDir, stats, updateNotice, cfg.DryRun, results)
	case cfg.LibraryDir != "":
		var newToLib, alreadyInLib, dropped int64
		if ingestMet != nil {
			newToLib = ingestMet.LinesUnique.Load()
			alreadyInLib = ingestMet.LinesSkippedByDest.Load()
			// creds the library's parser refused (non-ULP): closes the recap's
			// arithmetic, Unique == Added + already-in-library + dropped.
			dropped = ingestMet.LinesRejected.Load()
		}
		summary = renderIngestSummaryWithNotice(cfg.LibraryDir, ingestLibraryLines(ingestRes, ingestMet), newToLib, alreadyInLib, dropped, stats, ingestOutputPaths(ingestRes), updateNotice, cfg.DryRun, results)
	default:
		summary = renderFinalSummaryWithNotice(outPath, stats, updateNotice, results)
	}
	// Final recap frame header, harmonized with sfu's COMPLETE header (✓
	// phase tag left, elapsed clock flush right). Only frames whose header
	// slot is still empty: dry-run -od titles and the all-skip one-liner
	// keep their own first line. A failed history commit keeps the summary
	// silent — the state is signaled by exit code 3 and the issues log.
	if len(summary) > 2 && summary[1] == "" && historyNotRecordedMsg == "" {
		summary[1] = sflDoneHeader(cfg.DryRun, time.Since(startedOrNow(cfg)), termWidth())
	}
	frost := summaryFooterLines(termWidth(), updateNotice)
	// Env dest as an outside-box path footer (peer of Output/Library/Store), only
	// when files landed — lazy mkdir may leave no directory if nothing was copied.
	if stats.EnvCopied > 0 || stats.EnvDirsCopied > 0 {
		summary = spliceBeforeFooter(summary,
			renderSflPathFooter("Envs     ", []string{resolveEnvDir(cfg)}, sflMutedStyle), frost)
	}
	// The automatic issue log replaces the old encrypted-archive warning box:
	// when issues were captured and the log closed cleanly, one muted path
	// footer points at the full, untruncated TSV (optionally annotating how
	// many top-level archives had no matching password; no archive paths or
	// red block in the summary itself); when it could not be written, one
	// plain diagnostic line replaces it. Both print after the live monitor stopped.
	if line := issueFailureLine(issueRes); line != "" {
		fmt.Fprintln(os.Stderr, line)
	} else if block := issueFooterBlock(issueRes); block != nil {
		summary = spliceBeforeFooter(summary, block, frost)
	}
	for _, ln := range summary {
		fmt.Fprintln(os.Stderr, ln)
	}
	// All-history-skip has no recap box, so -del transparency rides here —
	// same "history: deleted N source(s):" block sfu prints after its ✓ line.
	if !cfg.DryRun && historyCommitErr == nil && historyAllSkipped(stats) && len(deletedPaths) > 0 {
		reportHistoryDeleted(os.Stderr, deletedPaths)
	}
	// A failed history write keeps its full error detail out of the box (the
	// box stays terse); one plain diagnostic line after the summary carries it,
	// mirroring the failed issue-log diagnostic.
	if historyCommitErr != nil {
		fmt.Fprintf(os.Stderr, "sfl: history commit failed: %v\n", historyCommitErr)
	}
	if cfg.Bell {
		bell.Ring()
	}
	// Exit-code policy (internal/exitcode): a completed run must not exit 0
	// unless every source succeeded. The summary above already shows what
	// failed; the exit code is the scriptable signal. Exception (2026-09-30,
	// user): the nothing-usable exits also print one terse reason line —
	// mirroring sfu — every other exit stays line-free.
	switch {
	case historyNotRecordedMsg != "":
		// Output committed but history not recorded: the state is recoverable
		// and the run wrote usable output, so per the F3 precedent ("a run
		// that wrote anything usable is now at worst 3") it exits 3, not 1.
		return &runOutcome{code: exitcode.Partial, msg: historyNotRecordedMsg}
	case noSources:
		// Nothing-usable exits print one terse reason line after the summary
		// (2026-09-30, user): the box reads like a normal completion at a
		// glance, and sfu names the reason for its exit 4 too. Other exits
		// stay line-free.
		msg := "no sources discovered in input"
		fmt.Fprintln(os.Stderr, "sfl: "+msg)
		return &runOutcome{code: exitcode.NothingUsable, msg: msg}
	case failedSources == len(results) && stats.Emitted == 0:
		// All-failed AND nothing written is the true nothing-usable class. A
		// salvaged truncated set wrote lines (e.g. the parts decoded before a
		// missing RAR volume): that output exists on disk, so the run is at
		// worst partial (3) — the exit code must not contradict the output.
		msg := fmt.Sprintf("all %d source(s) failed", failedSources)
		fmt.Fprintln(os.Stderr, "sfl: "+msg)
		return &runOutcome{code: exitcode.NothingUsable, msg: msg}
	case failedSources > 0:
		// The classic partial (some sources failed) and the salvaged-total
		// (every source failed but usable lines were written) share the same
		// code and message: output outranks the all-failed count.
		return &runOutcome{code: exitcode.Partial, msg: fmt.Sprintf("%d of %d source(s) failed", failedSources, len(results))}
	}
	return nil
}

// issueFooterBlock returns the muted Issues path footer for a successful log
// close that captured issues, or nil when there is nothing to show (zero
// issues, or a failed close — the caller prints issueFailureLine instead).
// When any top-level archive had no matching password, the label carries that
// count (singular/plural) so the summary stays minimal while still flagging
// the decrypt misses; nested password misses stay in the log only.
func issueFooterBlock(res issueLogResult) []string {
	if res.Err != nil || res.Count == 0 {
		return nil
	}
	label := "Issues   "
	if n := res.TopLevelPasswordNotFound; n > 0 {
		noun := "archives"
		if n == 1 {
			noun = "archive"
		}
		label = fmt.Sprintf("Issues (%d %s had no correct passwords given) ", n, noun)
	}
	return renderSflPathFooter(label, []string{res.Path}, sflMutedStyle)
}

// historyAllSkipped reports whether the run was a pure all-history-skip:
// every fingerprinted source was already completed, nothing was parsed,
// emitted, env-copied or failed, and no issues were recorded — the run gets
// sfu's one-line history summary instead of the recap box.
func historyAllSkipped(stats sflog.ExtractStats) bool {
	return stats.HistoryChecked > 0 &&
		stats.HistorySkipped == stats.HistoryChecked &&
		stats.Emitted == 0 &&
		stats.Credentials == 0 &&
		stats.EnvCopied == 0 &&
		stats.EnvDirsCopied == 0 &&
		stats.FailedSources == 0 &&
		len(stats.Issues) == 0
}

// historySkipSummaryLine mirrors sfu's history-only line byte-for-byte
// (cmd/sfu/history.go): "✓ history: N source(s) already completed · nothing
// to process", singular noun at N==1, same ok/count/muted role styling.
func historySkipSummaryLine(n int) string {
	noun := "sources"
	if n == 1 {
		noun = "source"
	}
	return fmt.Sprintf("%s %s %s %s",
		sflOkStyle.Render("✓"),
		sflOkStyle.Render("history:"),
		sflCountStyle.Render(fmt.Sprintf("%d", n)),
		sflMutedStyle.Render(fmt.Sprintf("%s already completed · nothing to process", noun)))
}

// reportHistoryDeleted mirrors sfu's history-only -del report
// (cmd/sfu/main.go): "history: deleted N source(s):" plus one indented path
// per removed source. Paths are printed as returned by deleteParsedSources.
func reportHistoryDeleted(w io.Writer, deleted []string) {
	if w == nil || len(deleted) == 0 {
		return
	}
	fmt.Fprintf(w, "history: deleted %d source(s):\n", len(deleted))
	for _, path := range deleted {
		fmt.Fprintln(w, "    "+path)
	}
}

// issueFailureLine returns the plain one-line diagnostic for a failed issue
// log, or "" when the log closed cleanly.
func issueFailureLine(res issueLogResult) string {
	if res.Err == nil {
		return ""
	}
	return fmt.Sprintf("sfl: could not write issue details: %v", res.Err)
}

// reportIssueLog surfaces the automatic issue log on exits that skip the
// summary (interrupts, extraction/finalize/ingest/delete failures): the same
// muted path footer the summary uses, or the plain write-failure diagnostic.
// Must be called only after the live monitor is stopped so frames never
// interleave.
func reportIssueLog(res issueLogResult) {
	if line := issueFailureLine(res); line != "" {
		fmt.Fprintln(os.Stderr, line)
		return
	}
	for _, ln := range issueFooterBlock(res) {
		fmt.Fprintln(os.Stderr, ln)
	}
}

func markEnvCopyIssues(results []sflog.SourceResult, issues []sflog.EnvCopyIssue) {
	for i := range results {
		for _, issue := range issues {
			if issue.Path == results[i].Path ||
				strings.HasPrefix(issue.Path, results[i].Path+"!") {
				results[i].HadIssue = true
				// preservation disabled 2026-09-30 (user): env-copy failures no longer
				// withhold history.
				// results[i].HistoryComplete = false
				break
			}
		}
	}
}

// spliceBeforeFooter inserts block into summary immediately before its trailing
// footer lines. footer is the exact block each renderer appends last, so its
// length locates the seam without coupling to a renderer's internal structure.
func spliceBeforeFooter(summary, block, footer []string) []string {
	cut := len(summary) - len(footer)
	if cut < 0 {
		return append(summary, block...)
	}
	out := make([]string, 0, len(summary)+len(block))
	out = append(out, summary[:cut]...)
	out = append(out, block...)
	return append(out, summary[cut:]...)
}

// openStagingULP creates the -od plaintext staging ULP file. Package-level so
// tests can inject a writer error without touching the filesystem.
var openStagingULP = func(path string) (io.WriteCloser, error) { return os.Create(path) }

// sink abstracts the credential destination so run() handles classic and -od
// modes uniformly. For classic the writer is the (optionally zstd) output file;
// for -od it is a temp ULP file later fed to sfu.
type sink struct {
	w         io.Writer
	file      io.Closer
	enc       *zstd.Encoder
	outPath   string
	ulpPath   string
	workDir   string
	protected []string
	// discarded guards abort's one-shot discard (no mutex needed: abort runs
	// only on the goroutine returning from run).
	discarded bool
}

func openSink(cfg runConfig) (*sink, error) {
	if cfg.LibraryDir != "" {
		libAbs, err := absClean(cfg.LibraryDir)
		if err != nil {
			return nil, err
		}
		// Decrypted-ULP staging. Prefer the library's parent so the library dir
		// stays clean, but if that isn't writable — e.g. -od /tmp derives parent
		// "/" — fall back to a subdir inside the library itself: always writable
		// (we ingest into it), same volume (a multi-GB ULP never hits tmpfs), and
		// invisible to the library reader. If neither works, fail in plain words.
		primary := cfg.TempDir
		if primary == "" {
			primary = filepath.Dir(libAbs)
		}
		workDir, err := makeStagingDir(primary, libAbs)
		if err != nil {
			return nil, err
		}
		// Holds the decrypted ULP; if a force-exit skips snk.cleanup the hint
		// must point the analyst at it for manual removal.
		ulpengine.RegisterCleanupPath(workDir)
		ulpPath := filepath.Join(workDir, "sfl_generated_ulp.txt")
		f, err := openStagingULP(ulpPath)
		if err != nil {
			_ = os.RemoveAll(workDir)
			return nil, err
		}
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
// Idempotent: a second call (e.g. from abort on a failure path) closes nothing
// and re-removes nothing.
func (s *sink) finalize(failed bool) error {
	// finalizeHook is a test seam (see main.go): fires before any closing so
	// tests can flip the signal seam mid-finalize. Production never sets it.
	if finalizeHook != nil {
		finalizeHook()
	}
	var err error
	if s.enc != nil {
		if cerr := s.enc.Close(); cerr != nil {
			err = cerr
		}
		s.enc = nil
	}
	if s.file != nil {
		if cerr := s.file.Close(); err == nil && cerr != nil {
			err = cerr
		}
		s.file = nil
	}
	if failed && s.workDir == "" {
		ulpengine.RemovePathLogged(s.outPath)
	}
	return err
}

// abort discards everything the sink created: closes the encoder/file, removes
// a classic output and the -od plaintext staging tree. It runs on failure paths
// between openSink and normal finalization — explicitly (extract/finalize/
// ingest errors) or via the discard defer armed right after openSink — so a
// failed run never leaves an empty classic output or staging behind.
// Idempotent; must not run once finalize has committed the output.
func (s *sink) abort() {
	if s.discarded {
		return
	}
	s.discarded = true
	_ = s.finalize(true)
	s.cleanup()
}

func (s *sink) cleanup() {
	if s.workDir != "" {
		ulpengine.RemoveTreeLogged(s.workDir)
		// unregister only when removal actually left it absent; a surviving
		// plaintext staging dir stays registered so a force-exit can still
		// clean it up
		if _, err := os.Stat(s.workDir); os.IsNotExist(err) {
			ulpengine.UnregisterCleanupPath(s.workDir)
		}
	}
}

func interruptCleanup() {
	ulpengine.FlushRegisteredCleanup()
}

func sweepOrphanWorkDirs(cfg runConfig) {
	for _, parent := range collectSweepParents(cfg) {
		if err := os.MkdirAll(parent, 0o755); err == nil {
			ulpengine.SweepStaleWorkDirs(parent, "")
		}
	}
}

func collectSweepParents(cfg runConfig) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		abs, err := absClean(dir)
		if err != nil {
			return
		}
		if _, ok := seen[abs]; ok {
			return
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	add(cfg.TempDir)
	// M-09: a dry-run consult (-odr) is read-only by contract — the sweep
	// must not MkdirAll (let alone clean) a library that is only being
	// previewed.
	if cfg.LibraryDir != "" && !cfg.DryRun {
		if lib, err := absClean(cfg.LibraryDir); err == nil {
			add(filepath.Dir(lib))
			add(lib)
		}
	}
	if cfg.OutputDir != "" {
		add(cfg.OutputDir)
	}
	return out
}

func buildEngine(cfg runConfig, passwords []string, prog *sflog.Progress, dbg *debugLogger, iss *issueLogger, spillDir string) *sflog.Engine {
	eng := &sflog.Engine{
		Workers:          cfg.Workers,
		NoURI:            cfg.NoURI,
		Loose:            cfg.Loose,
		Passwords:        passwords,
		Progress:         prog,
		TempDir:          spillDir,
		FollowedByIngest: cfg.LibraryDir != "",
		// Dedup extraction on the library's canonical host:login:password key
		// (matching the ingest parse mode) so "unique" collapses path-only
		// variants exactly as sfu/the library do — whether or not -od follows.
		// Strict by default; -loose widens both the dedup key and the ingest
		// parser so the two stay reconciled (the unique count matches what
		// ingest accepts).
		DedupKey: func(line string) (uint64, bool) {
			return ulpengine.DedupKeyForLine(line, cfg.Loose)
		},
	}
	if dbg != nil {
		eng.Debug = dbg.Event
	}
	if iss != nil {
		eng.OnIssue = iss.Record
	}
	return eng
}

// ingestToLibrary merges the generated ULP into the library in-process via the
// shared dedup engine, identical to `sfu -od <lib> <ulp>`. It installs an ingest
// view on prog so the live frame keeps rendering through the merge, and returns
// the resolved run + metrics so the caller can report the final library size.
// ingestTempDir resolves the shard-temp parent for the in-process ingest.
// An empty -tempdir makes the engine derive the temp parent from the output
// path — the library dir — and its MkdirAll (ingest.go) creates a library a
// dry run must never create: the M-09 contract is that a preview leaves a
// nonexistent library nonexistent. Dry-run therefore routes the shard temp
// to the platform temp dir, where the spill dir already defaults to. Real
// runs keep the engine default; an explicit -tempdir is honored in both
// modes.
func ingestTempDir(cfg runConfig) string {
	if cfg.DryRun && cfg.TempDir == "" {
		return os.TempDir()
	}
	return cfg.TempDir
}

func ingestToLibrary(ctx context.Context, cfg runConfig, ulpPath string, prog *sflog.Progress) (*ulpengine.Resolved, *ulpengine.Metrics, error) {
	ulpBytes := fileSizeOrZero(ulpPath)
	m := &ulpengine.Metrics{}
	var od atomic.Pointer[ulpengine.ODMetrics]
	// resolvedP feeds the dedup Progress row's worker denominator
	// (r.DedupWorkers), mirroring sfu's r.DedupWorkers access.
	var resolvedP atomic.Pointer[ulpengine.Resolved]

	// lastFrac clamps the bar monotonically. The closure is polled by the
	// monitor goroutine AND, with -json, by the stream's emitter — so the
	// captured sampler state is guarded by ingestViewMu. (Plain captured
	// floats would otherwise be a data race between the two consumers.)
	var ingestViewMu sync.Mutex
	var lastFrac float64
	var prevRegenAt time.Time
	var prevRegenBytes int64
	var prevWrittenAt time.Time
	var prevWrittenBytes int64
	prog.BeginIngest(func() sflog.IngestView {
		ingestViewMu.Lock()
		defer ingestViewMu.Unlock()
		odSnap := od.Load()
		regenBPS := 0.0
		if odSnap != nil {
			cur := odSnap.RegenBytesRead.Load()
			now := time.Now()
			if !prevRegenAt.IsZero() {
				if dt := now.Sub(prevRegenAt).Seconds(); dt >= 0.05 {
					regenBPS = float64(cur-prevRegenBytes) / dt
				}
			}
			prevRegenAt, prevRegenBytes = now, cur
		}
		writeBPS := 0.0
		{
			wr := m.BytesWritten.Load()
			now := time.Now()
			if !prevWrittenAt.IsZero() {
				if dt := now.Sub(prevWrittenAt).Seconds(); dt >= 0.05 {
					writeBPS = float64(wr-prevWrittenBytes) / dt
				}
			}
			prevWrittenAt, prevWrittenBytes = now, wr
		}
		v := ingestView(m, odSnap, resolvedP.Load(), ulpBytes, regenBPS, writeBPS)
		v.Fraction = monotonic(v.Fraction, &lastFrac)
		return v
	})

	// Capture the engine's ingest events (shard/dedup phases, -od scan/regen) to
	// a sibling of sfl's own debug log when -debug is set. nil DebugLog is a no-op
	// in the engine, so the unconditional pass-through is safe.
	elog := newIngestDebugLog(cfg)
	rr := newIngestRejectRecorder(cfg)
	// ExitWithCode (fatal/usage) and ForceExit (second Ctrl-C, cleanup
	// timeout) end in os.Exit, which skips the deferred close below. The
	// ingest log and the reject recorder are both buffered (the recorder is
	// never flushed mid-run), so arm the registry's pre-exit flush while they
	// are open and detach when they close — after ingest returns there is no
	// seam left that could exit with either still open, and a stale hook
	// would flush a closed file (silently, but pointlessly).
	reg.SetExitFlush(func() {
		elog.Flush()
		rr.Flush()
	})
	defer func() {
		reg.ClearExitFlush()
		_ = elog.Close()
		_ = rr.Close()
	}()

	opts := ulpengine.IngestOptions{
		ULPPath:    ulpPath,
		LibraryDir: cfg.LibraryDir,
		Workers:    cfg.Workers,
		TempDir:    ingestTempDir(cfg),
		NoURI:      cfg.NoURI,
		Loose:      cfg.Loose,
		RunStarted: startedOrNow(cfg),
		// The -od archive names itself after the same run stamp as the ULP
		// output and the -env dir; no second slug is generated here.
		RunStamp: cfg.RunStamp,
		Debug:    elog,
		Reject:   rr,
		DryRun:   cfg.DryRun,
		OnResolved: func(r *ulpengine.Resolved) {
			if r != nil {
				od.Store(r.OdMetrics)
				resolvedP.Store(r)
			}
		},
	}
	r, err := ulpengine.Ingest(ctx, opts, m)
	return r, m, err
}

func ingestOutputPaths(res *ulpengine.Resolved) []string {
	if res == nil {
		return nil
	}
	if len(res.OutputPaths) > 0 {
		return res.OutputPaths
	}
	return nil
}

func ingestHistoryOutputPaths(res *ulpengine.Resolved) []string {
	return ulpengine.DurableArtifacts(ingestOutputPaths(res))
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

// ingestDebugDir returns where the ingest-side debug artifacts live: the
// library dir for -od, else the working directory — the same policy sfl's own
// debug log follows. A dry-run consult (-odr, or [sfl] odr=true) is read-only
// by contract: the library dir is excluded there (mirroring issueLogDir), so
// the ingest debug/reject artifacts land in the CWD fallback and a previewed
// library is never MkdirAll'd nor polluted (the M-09 batch-4 fix defect).
// Returns "" when the dir cannot be created.
func ingestDebugDir(cfg runConfig) string {
	dir := cfg.LibraryDir
	if dir == "" || cfg.DryRun {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// newIngestDebugLog opens an engine debug log for the in-process ingest next to
// sfl's own debug log (library dir for -od), or returns nil when -debug is off.
// A nil *ulpengine.DebugLog is safe to pass and Close.
func newIngestDebugLog(cfg runConfig) *ulpengine.DebugLog {
	if !cfg.Debug {
		return nil
	}
	dir := ingestDebugDir(cfg)
	if dir == "" {
		return nil
	}
	f, _, err := ulpengine.CreateArtifactFile(dir, "sfl_ingest_debug_"+startedOrNow(cfg).Format("20060102_150405"), ".log", 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sfl: ingest debug log disabled: %v\n", err)
		return nil
	}
	return ulpengine.NewDebugLogFile(f)
}

// newIngestRejectRecorder opens the ingest reject artifact (the same rows
// sfu's -debug-reject writes: one per rejected line, "[password>64] ..."-style
// tags when a named reason owns the reject) next to the ingest debug log, or
// returns nil when -debug-reject is off. A nil *ulpengine.RejectRecorder is a
// no-op in the engine, so it is safe to pass unconditionally.
func newIngestRejectRecorder(cfg runConfig) *ulpengine.RejectRecorder {
	if !cfg.DebugReject {
		return nil
	}
	dir := ingestDebugDir(cfg)
	if dir == "" {
		return nil
	}
	f, _, err := ulpengine.CreateArtifactFile(dir, "sfl_ingest_rejected_"+startedOrNow(cfg).Format("20060102_150405"), ".txt", 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sfl: ingest reject recorder disabled: %v\n", err)
		return nil
	}
	return ulpengine.NewRejectRecorderFile(f)
}

// ingestRejectBreakdown renders the nonzero per-reason ingest reject tallies
// as a parenthesized suffix for the history-not-recorded message, e.g.
// " (password>64: 112000, malformed: 1293)". Zero reasons are omitted; an
// all-zero tally (theoretically impossible: the reasons sum into the
// rejected total) returns "" so the message stays total-only.
func ingestRejectBreakdown(m *ulpengine.Metrics) string {
	parts := make([]string, 0, 4)
	if n := m.LinesTooLong.Load(); n != 0 {
		parts = append(parts, fmt.Sprintf("tooLong: %d", n))
	}
	if n := m.LinesPasswordTooLong.Load(); n != 0 {
		parts = append(parts, fmt.Sprintf("password>64: %d", n))
	}
	if n := m.LinesMalformed.Load(); n != 0 {
		parts = append(parts, fmt.Sprintf("malformed: %d", n))
	}
	if n := m.LinesUnrepresentable.Load(); n != 0 {
		parts = append(parts, fmt.Sprintf("unrepresentable: %d", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// ingestView snapshots the dedup engine's atomics into the icy INGESTING frame.
func ingestView(m *ulpengine.Metrics, od *ulpengine.ODMetrics, res *ulpengine.Resolved, ulpBytes int64, regenBPS, writeBPS float64) sflog.IngestView {
	frac, status := ingestProgress(m, od, ulpBytes)
	v := sflog.IngestView{
		Fraction:          frac,
		Status:            status,
		EnginePhase:       m.Phase.Load(),
		ULPBytes:          ulpBytes,
		BytesRead:         m.BytesRead.Load(),
		LinesRead:         m.LinesRead.Load(),
		ShowMerge:         ingestShowMerge(m),
		Unique:            m.LinesUnique.Load(),
		Skipped:           m.LinesSkippedByDest.Load(),
		BucketsDone:       m.BucketsDone.Load(),
		BucketsTotal:      m.BucketsTotal.Load(),
		BucketsBytesRead:  m.BucketsBytesRead.Load(),
		BucketsBytesTotal: m.BucketsBytesTotal.Load(),
		BusyWorkers:       m.BusyWorkers.Load(),
		RegenBPS:          regenBPS,
		WriteBPS:          writeBPS,
	}
	if res != nil {
		v.DedupWorkers = int32(res.DedupWorkers)
	}
	if od != nil {
		v.ODPhase = int32(od.Phase.Load())
		v.ArchivesTotal = od.ArchivesTotal.Load()
		v.FilesTotal = od.FilesTotal.Load()
		v.ArchivesSkipped = od.ArchivesSkipped.Load()
		v.PartsRegenDone = od.PartsRegenDone.Load()
		v.PartsRegenTotal = od.PartsRegenTotal.Load()
		v.PartsUpgradeTotal = od.PartsUpgradeTotal.Load()
		v.RegenBytesRead = od.RegenBytesRead.Load()
		v.RegenBytesTotal = od.RegenBytesTotal.Load()
		v.LibraryKeys = od.KeysTotalEstimate.Load()
		v.KeysLoaded = od.KeysLoaded.Load()
		v.Workers = snapshotIngestWorkers(od)
	}
	return v
}

func ingestShowMerge(m *ulpengine.Metrics) bool {
	if m == nil {
		return false
	}
	switch m.Phase.Load() {
	case ulpengine.PhaseDedup, ulpengine.PhaseDone:
		return true
	}
	if m.LinesUnique.Load()+m.LinesSkippedByDest.Load() > 0 {
		return true
	}
	return m.BucketsBytesRead.Load() > 0
}

func snapshotIngestWorkers(od *ulpengine.ODMetrics) []sflog.IngestWorker {
	if od == nil {
		return nil
	}
	ph := ulpengine.ODPhase(od.Phase.Load())
	if ph != ulpengine.ODPhaseRegen {
		return nil
	}
	// Terminal-independent cap: the JSON stream must not depend on the
	// terminal height; the TUI panel slices to the terminal when rendering.
	active := od.ActiveWorkers(sflog.MaxWorkerRows)
	if len(active) == 0 {
		return nil
	}
	out := make([]sflog.IngestWorker, 0, len(active))
	for _, ws := range active {
		namePtr := ws.ArchivePath.Load()
		if namePtr == nil {
			continue
		}
		out = append(out, sflog.IngestWorker{
			Archive:    *namePtr,
			PartIdx:    ws.PartIdx.Load(),
			PartsTotal: ws.PartsTotal.Load(),
			BytesDone:  ws.BytesDone.Load(),
			BytesTotal: ws.BytesTotal.Load(),
			Committing: ws.Committing.Load(),
		})
	}
	return out
}

// ingestProgress maps the engine's phase + byte counters onto a 0..1 bar.
//
// The -od pipeline is concurrent and re-orders phases: shard (reading the small
// ULP) runs alongside phase-0 regen, and m.Phase flips BACK to phasePhase0 after
// shard while regen drains (see internal/ulpengine/pipeline.go). Keying purely on
// m.Phase would shoot the bar to ~70% on the fast shard, then snap it back to ~5%
// for the slow regen. So the pre-dedup region [0.03, 0.65] is driven by the
// dominant cold-library cost (regen) when present, else by the ULP read, both of
// which advance monotonically. Dedup owns [0.65, 1.0], done 1.0.
// The caller's BeginIngest closure also clamps the result monotonically as a
// belt-and-suspenders against any residual reorder.
func ingestProgress(m *ulpengine.Metrics, od *ulpengine.ODMetrics, ulpBytes int64) (float64, string) {
	switch m.Phase.Load() {
	case ulpengine.PhaseDone:
		return 1.0, "done"
	case ulpengine.PhaseDedup:
		frac := 0.70
		if tot := m.BucketsBytesTotal.Load(); tot > 0 {
			frac = 0.65 + 0.35*clampFrac(float64(m.BucketsBytesRead.Load())/float64(tot))
		}
		return frac, tuistat.LibraryMerging
	default:
		// PhaseInit / PhasePhase0 / PhaseShard: the concurrent pre-dedup region.
		// Regen is the long pole on a cold library and runs concurrently with the
		// quick shard, so prefer it; ignore the shard's fast climb that would
		// otherwise invert the bar when m.Phase returns to phasePhase0.
		if od != nil {
			if ulpengine.ODPhase(od.Phase.Load()) == ulpengine.ODPhaseUpgrade {
				frac := 0.03
				if tot := od.PartsRegenTotal.Load(); tot > 0 {
					frac = 0.03 + 0.62*clampFrac(float64(od.PartsRegenDone.Load())/float64(tot))
				}
				return frac, tuistat.LibraryUpgrading
			}
			if tot := od.RegenBytesTotal.Load(); tot > 0 {
				return 0.03 + 0.62*clampFrac(float64(od.RegenBytesRead.Load())/float64(tot)),
					tuistat.LibraryPreparing
			}
		}
		if ulpBytes > 0 {
			return 0.03 + 0.62*clampFrac(float64(m.BytesRead.Load())/float64(ulpBytes)),
				"reading extracted credentials…"
		}
		return 0.03, tuistat.LibraryScanning
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
	// dry-run writes nothing, so the library total is the pre-run size only;
	// LinesUnique here is the would-be-added count, not a real addition.
	if m != nil && !r.Cfg.DryRun {
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
	for _, ln := range renderInterruptSummary(time.Since(startedOrNow(cfg)), ulpengine.SnapshotCleanupLog()) {
		fmt.Fprintln(os.Stderr, ln)
	}
}

// ensureRunStamp resolves the one shared run stamp ("YYYYMMDD_<six-char-id>",
// no time-of-day) before any output path is computed. An injected stamp (tests)
// wins; otherwise a single crypto/rand id is drawn here and reused by the ULP
// output name, the -env dir, and the -od ingest archive. The entropy
// error is returned as-is — there is no HHMMSS fallback.
func ensureRunStamp(cfg *runConfig) error {
	if cfg.RunStamp != "" {
		return nil
	}
	id, err := ulpengine.NewRunID()
	if err != nil {
		return fmt.Errorf("run id: %w", err)
	}
	cfg.RunStamp = ulpengine.RunStamp(startedOrNow(*cfg), id)
	return nil
}

// resolveEnvDir returns the flat -env directory path
// (<dest>/sfl_<runstamp>_secrets/). OutputDir takes precedence, then
// LibraryDir, then ".". It does NOT create the directory: the
// copier creates it lazily on the first successful write so an empty run leaves
// no empty folder behind. The run stamp is shared with the ULP output name.
func resolveEnvDir(cfg runConfig) string {
	var dest string
	switch {
	case cfg.OutputDir != "":
		dest = cfg.OutputDir
	case cfg.LibraryDir != "":
		dest = cfg.LibraryDir
	default:
		dest = "."
	}
	return filepath.Join(dest, "sfl_"+cfg.RunStamp+"_secrets")
}

// usageError marks an error that is deterministic argv-shape validation: the
// caller routes it through usagef (exit 2) instead of fatalf (exit 1). M-11.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

type jsonOutTargetError struct{ err error }

func (e *jsonOutTargetError) Error() string { return e.err.Error() }
func (e *jsonOutTargetError) Unwrap() error { return e.err }

// validateJSONOutTarget rejects targets that would overwrite any input or
// durable artifact. It performs no filesystem writes, allowing this check to
// run before sink, history, and stream creation.
func validateJSONOutTarget(cfg runConfig) error {
	if cfg.JSONOut == "" || cfg.JSONOut == "-" {
		return nil
	}
	target, err := pathident.CanonicalProspective(cfg.JSONOut)
	if err != nil {
		return &jsonOutTargetError{fmt.Errorf("invalid -json target %q: %w", cfg.JSONOut, err)}
	}
	targetInfo, targetStatErr := os.Stat(cfg.JSONOut)
	if targetStatErr != nil && !os.IsNotExist(targetStatErr) {
		return &jsonOutTargetError{fmt.Errorf("invalid -json target %q: %w", cfg.JSONOut, targetStatErr)}
	}
	same := func(path string) bool {
		canonical, e := pathident.CanonicalProspective(path)
		if e == nil && canonical == target {
			return true
		}
		if targetInfo == nil {
			// The target does not exist as spelled — including the shape
			// where its final component is a dangling symlink: the canonical
			// spelling of such a target is the link path itself, but the
			// stream's create follows the chain onto the referent. Compare
			// the referent chain against the protected path too (the same
			// treatment the history segment gets from
			// history.JSONOutHistoryCollision).
			return pathident.LinkRefersTo(cfg.JSONOut, path)
		}
		info, e := os.Stat(path)
		return e == nil && os.SameFile(targetInfo, info)
	}
	reject := func(label, path string) error {
		return &jsonOutTargetError{fmt.Errorf("invalid -json target %q: overlaps %s %q", cfg.JSONOut, label, path)}
	}
	if same(cfg.Input) {
		return reject("input", cfg.Input)
	}
	if inputInfo, e := os.Stat(cfg.Input); e == nil && inputInfo.IsDir() {
		if inputDir, e := pathident.CanonicalProspective(cfg.Input); e == nil && pathident.WithinDir(target, inputDir) {
			return reject("input directory", cfg.Input)
		}
		// A dangling link from outside the input directory whose referent
		// lands inside it would smuggle a stream-created file into the
		// scanned tree; the plain WithinDir compare cannot see that landing.
		if pathident.LinkRefersWithinDir(cfg.JSONOut, cfg.Input) {
			return reject("input directory", cfg.Input)
		}
	}
	if cfg.Password != "" {
		if _, e := os.Stat(cfg.Password); e == nil && same(cfg.Password) {
			return reject("password list", cfg.Password)
		}
	}
	if cfg.HistoryPath != "" {
		// The store resolves a directory -history-path to DIR/history.sqlite3
		// (plus -wal/-shm sidecars) at open time, so the target must be
		// compared against the effective endpoints, not the raw spelling.
		// history.JSONOutHistoryCollision owns that comparison (including a
		// -json target that is a symlink, possibly dangling, onto the
		// database: the stream's create follows the link).
		if hit := history.JSONOutHistoryCollision(cfg.JSONOut, cfg.HistoryPath); hit != "" {
			return reject("history database", hit)
		}
	}
	if cfg.LibraryDir != "" {
		if lib, e := pathident.CanonicalProspective(cfg.LibraryDir); e == nil && pathident.WithinDir(target, lib) {
			return reject("library directory", cfg.LibraryDir)
		}
		// Same dangling-link shape as the input directory: a referent landing
		// inside the library pollutes it or, once the file exists, is a
		// library artifact the next run trusts.
		if pathident.LinkRefersWithinDir(cfg.JSONOut, cfg.LibraryDir) {
			return reject("library directory", cfg.LibraryDir)
		}
	}
	outDir := cfg.OutputDir
	if outDir == "" {
		outDir = "."
	}
	classic := filepath.Join(outDir, "sfl_"+cfg.RunStamp+".txt")
	if cfg.Compress {
		classic += ".zst"
	}
	if same(classic) {
		return reject("classic output", classic)
	}
	return nil
}

func createOutputPath(cfg runConfig) (string, error) {
	if cfg.OutputDir == "" {
		cfg.OutputDir = "."
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return "", err
	}
	name := "sfl_" + cfg.RunStamp + ".txt"
	if cfg.Compress {
		name += ".zst"
	}
	return filepath.Join(cfg.OutputDir, name), nil
}

// preflightOutputDir validates an output dir flag value before the pipeline
// starts: -o gets the dir-hint guard (reject -o cleaned.txt), -od/-odr the
// always-dir guard, and any existing file at the path is rejected with a
// friendly message instead of a raw ENOTDIR from MkdirAll. Creation is left
// to the sink setup after all run validation has completed.
func preflightOutputDir(flagName, dir string) error {
	if _, _, err := outdir.ResolveDir(flagName, dir); err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve output dir: %w", err)
	}
	// Preflight is validation only; sink setup creates missing dirs for real
	// runs, while dry-run setup remains non-mutating.
	return outdir.EnsureReady(flagName, abs, false)
}

// makeStagingDir creates a 0700 dir to hold the decrypted ULP. It tries primary
// first, then a subdir inside the library (guaranteed writable, same volume).
// On total failure it returns a plain-language error listing every path tried
// and how to fix it.
func makeStagingDir(primary, libDir string) (string, error) {
	tried := []string{primary}
	if libDir != "" && libDir != primary {
		tried = append(tried, libDir)
	}
	var lastErr error
	for _, dir := range tried {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			lastErr = err
			continue
		}
		workDir, err := os.MkdirTemp(dir, "sfl-od-*")
		if err != nil {
			lastErr = err
			continue
		}
		return workDir, nil
	}
	return "", fmt.Errorf(
		"could not create a temporary folder for decrypted logs.\n"+
			"  tried: %s\n"+
			"  reason: %v\n"+
			"  fix: pass -temp-dir <a writable directory>, or set -od to a writable path",
		strings.Join(tried, ", "), lastErr)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfl: "+format+"\n", args...)
	reg.ExitWithCode(1)
}

func usagef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfl: "+format+"\n", args...)
	flag.Usage()
	reg.ExitWithCode(2)
}
