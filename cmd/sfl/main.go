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
	"github.com/snowx-dev/SnowFastULP/internal/secrets"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// reg is the shared terminal restore/exit registry: the live screen registers
// its teardown via Set, and every exit path routes through it so the
// alt-screen is always left cleanly. ulpengine.PrintManualCleanupHint prints
// stranded scratch paths on a force-exit (second Ctrl-C / cleanup timeout).
var reg = termctl.New(os.Stderr, ulpengine.PrintManualCleanupHint)

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
	ErrFile       bool
	NoUpdateCheck bool
	Secrets       bool
	SecretsPath   string
	SecretsAllow  []string
	SecretsDeny   []string
	Env           bool
	EnvRoot       string // populated at run start when -env is set
	UpdateChecker *selfupdate.Checker
	Started       time.Time
	// DryRun (-odr): run the full extract+ingest pipeline but write nothing
	// to the library; the summary reports what would have been added.
	DryRun bool
	// ForcePlainTUI is set when VT processing can't be enabled (legacy Windows
	// console), forcing plain output so ANSI escapes never leak as raw text.
	ForcePlainTUI bool
}

func main() {
	// vtOK is false only on a legacy Windows console that can't render ANSI;
	// it flows into runConfig to force plain mode so escapes never leak.
	vtOK := console.EnableVT()
	started := time.Now()

	flag.Usage = func() { printHelp(filepath.Base(os.Args[0]), os.Stderr) }

	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastLog %s\n", version.String)
		return
	}
	if cliargs.IsHelpRequest(os.Args[1:]) {
		printHelp(filepath.Base(os.Args[0]), os.Stdout)
		reg.ExitWithCode(0)
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
	outDryRun := flag.String("odr", "", "like -od but writes nothing: preview what a run would add to the library")
	password := flag.String("p", "", "archive password or password-list file")
	workers := flag.Int("workers", 0, "parser/archive worker count (0=auto)")
	workersAlias := flag.Int("j", 0, "alias for -workers")
	tempDir := flag.String("temp-dir", "", "directory for temp files")
	noTUI := flag.Bool("no-tui", false, "disable live TUI")
	zst := flag.Bool("zst", false, "compress classic output with zstd")
	delSrc := flag.Bool("del", false, "delete source files after success")
	noURI := flag.Bool("no-uri", false, "emit host:login:password")
	debug := flag.Bool("debug", false, "write structured debug log")
	errFile := flag.Bool("err", false, "write the full, untruncated issue list to a file")
	secretsOn := flag.Bool("secrets", false, "scan non-credential files for secrets (API keys, tokens) into a sqlite store")
	secretsPath := flag.String("secrets-path", "", "path to the secrets sqlite DB (default: <output>/sfl-secrets.sqlite)")
	secretsPathAlias := flag.String("sec-path", "", "alias for -secrets-path")
	var secretsAllow, secretsDeny []string
	flag.Var(stringAccum{&secretsAllow}, "secrets-allow", "glob of titus rule IDs to keep (e.g. 'np.aws.*'); repeatable. Empty = all rules.")
	flag.Var(stringAccum{&secretsDeny}, "secrets-deny", "glob of titus rule IDs to drop (e.g. 'np.aws.3'); repeatable. Wins over -secrets-allow.")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable background update check")
	envOn := flag.Bool("env", false, "copy env/key files into <dest>/env/<timestamp>/<log>/<victim>/ with per-victim index.txt")

	flagArgs, positional := cliargs.SplitPositional(config.StripConfigArgv(os.Args[1:]), flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		reg.ExitWithCode(2)
	}
	visited := config.NewVisited()
	// Accept -j as an alias for -workers (sfs uses -j) so the same invocation
	// works across all three CLIs; explicit -workers wins.
	visited.ResolveIntAlias(workers, workersAlias, "workers", "j")
	visited.ResolveStringAlias(secretsPath, secretsPathAlias, "secrets-path", "sec-path")
	var odrCfg bool
	if err := fileCfg.ApplySFL(visited, config.SFLFlags{
		O: out, OD: outDedup, ODR: &odrCfg, TempDir: tempDir, Password: password,
		SecretsPath: secretsPath,
		Workers:     workers,
		NoTUI:       noTUI, Zst: zst, Del: delSrc, NoURI: noURI,
		Debug: debug, NoUpdateCheck: noUpdateCheck,
		Secrets:      secretsOn,
		SecretsAllow: &secretsAllow,
		SecretsDeny:  &secretsDeny,
		Env:          envOn,
	}); err != nil {
		fatalf("%v", err)
	}

	inputArg := resolveInputArg(fileCfg, positional)
	if strings.TrimSpace(inputArg) == "" {
		fmt.Fprintln(os.Stderr, "sfl: no input path provided; set [sfl].input in your config or pass INPUT_PATH on the CLI")
		fmt.Fprintln(os.Stderr)
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
	cfg := runConfig{
		Input: inputArg, OutputDir: *out, LibraryDir: libraryDir, Password: *password,
		TempDir: *tempDir, Workers: w, Compress: *zst, DeleteSources: *delSrc,
		NoURI: *noURI, NoTUI: *noTUI, Debug: *debug, ErrFile: *errFile, NoUpdateCheck: *noUpdateCheck,
		Secrets: *secretsOn, SecretsPath: *secretsPath,
		SecretsAllow: secretsAllow, SecretsDeny: secretsDeny,
		Env: *envOn,
		Started: started, ForcePlainTUI: !vtOK,
		DryRun: dryRun,
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
func run(cfg runConfig) error {
	// Keep a redirected summary/log free of ANSI; the live frame and summary
	// both target stderr, so color follows stderr's TTY status.
	applyStderrColorProfile()

	sweepOrphanWorkDirs(cfg)

	ctx, cancel, signaled := reg.SignalContext()
	defer cancel()

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

	snk, err := openSink(cfg)
	if err != nil {
		return err
	}
	dbg.Header(cfg, len(passwords), snk.outPath)

	prog := sflog.NewProgress()
	prog.SetDryRun(cfg.DryRun)
	tuiOff := cfg.NoTUI || !stderrIsTTY() || cfg.ForcePlainTUI
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
	// graceful interrupt paths below can clean up explicitly before reg.ExitWithCode
	// — which os.Exits and therefore skips this defer — without any double-free.
	removeSpill := func() { ulpengine.RemoveTreeLogged(spillDir) }
	defer removeSpill()

	eng := buildEngine(cfg, passwords, prog, dbg, iss, spillDir)

	var envCopier *sflog.EnvCopier
	if cfg.Env {
		envRoot, err := resolveEnvRoot(cfg)
		if err != nil {
			return fmt.Errorf("env: %w", err)
		}
		cfg.EnvRoot = envRoot
		if !cfg.DryRun {
			envCopier = sflog.NewEnvCopier(envRoot, prog, 0)
			envCopier.Start()
			prog.EnableEnv()
			eng.EnvCopier = envCopier
		}
	}

	// Optional secrets scanning runs as a side channel during extraction: the
	// engine tees non-credential member bytes to the Titus-backed sink, which
	// accumulates + dedupes into the SQLite store. Closed right after extraction
	// (and via defer on any early return) so its stats reach the summary.
	var secretsStats secrets.Stats
	var closeSecrets func()
	if cfg.Secrets {
		sink, closeFn, serr := buildSecretSink(
			resolveSecretsPath(cfg.SecretsPath, cfg.OutputDir, cfg.LibraryDir), cfg.Workers,
			secrets.RuleFilter{Allow: cfg.SecretsAllow, Deny: cfg.SecretsDeny})
		if serr != nil {
			return fmt.Errorf("secrets: %w", serr)
		}
		eng.SecretSink = sink
		var once sync.Once
		closeSecrets = func() {
			once.Do(func() {
				st, cerr := closeFn()
				secretsStats = st
				dbg.Event("secrets: new=%d existing=%d dup=%d deduped=%d", st.New, st.Existing, st.DupInRun, st.Deduped)
				if cerr != nil {
					dbg.Event("secrets: close error: %v", cerr)
				}
			})
		}
		defer closeSecrets()
	}

	stats, results, extractErr := eng.Run(ctx, cfg.Input, snk.w)
	if envCopier != nil {
		es := envCopier.Close()
		stats.EnvCopied = es.Copied
		stats.EnvContextCopied = es.ContextCopied
		stats.EnvSkippedOverCap = es.SkippedOverCap
		stats.EnvWriteErrors = es.WriteErrors
		dbg.Event("env: copied=%d context=%d skipped=%d errors=%d root=%q",
			es.Copied, es.ContextCopied, es.SkippedOverCap, es.WriteErrors, cfg.EnvRoot)
	}
	if closeSecrets != nil {
		// Flip the live frame to a dedicated "finalizing secrets" phase while the
		// store drains its last batch and checkpoints the WAL, so the hand-off
		// from 100% extraction to the summary never reads as a stall. Skipped on
		// error, where the frame is torn down below instead.
		if extractErr == nil {
			prog.BeginSecretsFinalize()
		}
		closeSecrets() // flush + capture stats before the summary (idempotent)
	}
	dbg.Completion(stats)
	dbg.Issues(stats)
	if extractErr != nil {
		dbg.Event("extract ended early: %v (signaled=%v)", extractErr, signaled())
	}
	finalizeErr := snk.finalize(extractErr != nil)

	// Extraction/finalize failures tear the live frame down before any stderr so
	// frames never interleave with the error.
	if extractErr != nil {
		if signaled() {
			interruptCleanup()
		}
		stopMonitor()
		if signaled() {
			printInterruptSummary(cfg)
			reg.ExitWithCode(130)
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
			if err != nil {
				dbg.Event("ingest: ended early: %v (signaled=%v)", err, signaled())
				if signaled() {
					interruptCleanup()
				}
				stopMonitor()
				if signaled() {
					printInterruptSummary(cfg)
					reg.ExitWithCode(130)
				}
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

	if cfg.DeleteSources && !cfg.DryRun {
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
		summary = renderNoIngestSummaryWithNotice(cfg.LibraryDir, stats, cfg.EnvRoot, updateNotice, cfg.DryRun)
	case cfg.LibraryDir != "":
		var newToLib, alreadyInLib, dropped int64
		if ingestMet != nil {
			newToLib = ingestMet.LinesUnique.Load()
			alreadyInLib = ingestMet.LinesSkippedByDest.Load()
			// creds the library's parser refused (non-ULP): closes the recap's
			// arithmetic, Unique == Added + already-in-library + dropped.
			dropped = ingestMet.LinesRejected.Load()
		}
		summary = renderIngestSummaryWithNotice(cfg.LibraryDir, ingestLibraryLines(ingestRes, ingestMet), newToLib, alreadyInLib, dropped, stats, ingestOutputPaths(ingestRes), cfg.EnvRoot, updateNotice, cfg.DryRun)
	default:
		summary = renderFinalSummaryWithNotice(outPath, stats, cfg.EnvRoot, updateNotice)
	}
	if cfg.Secrets {
		// Slot the secrets recap box just above the frost footer so it reads as
		// a peer of the credential summary. The footer is the deterministic
		// trailing block every renderer ends with, so splice ahead of it rather
		// than depending on any single renderer's internal layout.
		block := renderSecretsBlock(secretsStats,
			resolveSecretsPath(cfg.SecretsPath, cfg.OutputDir, cfg.LibraryDir), termWidth())
		summary = spliceBeforeFooter(summary, block, summaryFooterLines(termWidth(), updateNotice))
	}
	// A prominent encrypted-archive warning comes BEFORE the COMPLETE summary so
	// a "0 ULP" run can't read as empty when the real cause is a missing password.
	// Printed on the normal screen (alt-screen already torn down) like the summary.
	if w := renderEncryptedWarning(stats, cfg.Password != "", termWidth()); len(w) > 0 {
		for _, ln := range w {
			fmt.Fprintln(os.Stderr, ln)
		}
	}
	for _, ln := range summary {
		fmt.Fprintln(os.Stderr, ln)
	}
	return nil
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
		f, err := os.Create(ulpPath)
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
		ulpengine.RemovePathLogged(s.outPath)
	}
	return err
}

func (s *sink) cleanup() {
	if s.workDir != "" {
		ulpengine.RemoveTreeLogged(s.workDir)
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
	if cfg.LibraryDir != "" {
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
		Passwords:        passwords,
		Progress:         prog,
		TempDir:          spillDir,
		FollowedByIngest: cfg.LibraryDir != "",
		// Dedup extraction on the library's canonical host:login:password key
		// (strict parse, matching the ingest) so "unique" collapses path-only
		// variants exactly as sfu/the library do — whether or not -od follows.
		DedupKey: func(line string) (uint64, bool) {
			return ulpengine.DedupKeyForLine(line, false)
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
func ingestToLibrary(ctx context.Context, cfg runConfig, ulpPath string, prog *sflog.Progress) (*ulpengine.Resolved, *ulpengine.Metrics, error) {
	ulpBytes := fileSizeOrZero(ulpPath)
	m := &ulpengine.Metrics{}
	var od atomic.Pointer[ulpengine.ODMetrics]

	// lastFrac clamps the bar monotonically. The closure is polled only by the
	// monitor goroutine, so a plain captured float needs no synchronization.
	var lastFrac float64
	var prevRegenAt time.Time
	var prevRegenBytes int64
	prog.BeginIngest(func() sflog.IngestView {
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
		v := ingestView(m, odSnap, ulpBytes, regenBPS)
		v.Fraction = monotonic(v.Fraction, &lastFrac)
		return v
	})

	// Capture the engine's ingest events (shard/dedup phases, -od scan/regen) to
	// a sibling of sfl's own debug log when -debug is set. nil DebugLog is a no-op
	// in the engine, so the unconditional pass-through is safe.
	elog := newIngestDebugLog(cfg)
	defer func() { _ = elog.Close() }()

	opts := ulpengine.IngestOptions{
		ULPPath:    ulpPath,
		LibraryDir: cfg.LibraryDir,
		Workers:    cfg.Workers,
		TempDir:    cfg.TempDir,
		NoURI:      cfg.NoURI,
		RunStarted: startedOrNow(cfg),
		Debug:      elog,
		DryRun:     cfg.DryRun,
		OnResolved: func(r *ulpengine.Resolved) {
			if r != nil {
				od.Store(r.OdMetrics)
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

// ingestView snapshots the dedup engine's atomics into the icy INGESTING frame.
func ingestView(m *ulpengine.Metrics, od *ulpengine.ODMetrics, ulpBytes int64, regenBPS float64) sflog.IngestView {
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
		RegenBPS:          regenBPS,
	}
	if od != nil {
		v.ODPhase = int32(od.Phase.Load())
		v.ArchivesTotal = od.ArchivesTotal.Load()
		v.PartsRegenDone = od.PartsRegenDone.Load()
		v.PartsRegenTotal = od.PartsRegenTotal.Load()
		v.RegenBytesRead = od.RegenBytesRead.Load()
		v.RegenBytesTotal = od.RegenBytesTotal.Load()
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
	cap := sflIngestRegenRowCap(termHeight(), od.WorkerCount())
	active := od.ActiveWorkers(cap)
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

func resolveEnvRoot(cfg runConfig) (string, error) {
	dest := cfg.OutputDir
	if cfg.LibraryDir != "" {
		dest = cfg.LibraryDir
	}
	if dest == "" {
		dest = "."
	}
	started := cfg.Started
	if started.IsZero() {
		started = time.Now()
	}
	stamp := started.Format("200601021504")
	root := filepath.Join(dest, "env", stamp)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
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
	fmt.Fprintf(os.Stderr, "sfl: "+format+"\n\n", args...)
	flag.Usage()
	reg.ExitWithCode(2)
}
