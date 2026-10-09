package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// reg is the shared terminal restore/exit registry: the live frame registers
// its teardown via Set, and every exit path (graceful ExitWithCode, force-exit
// on a second Ctrl-C, cleanup timeout, fatal/usage) routes through it so the
// alt-screen is always left cleanly. ulpengine.PrintManualCleanupHint prints
// stranded scratch paths on a force-exit.
var reg = termctl.New(os.Stderr, ulpengine.PrintManualCleanupHint)

var bellEnabled bool

// outputMode captures which output sink the run targets and whether it's a
// dry-run preview. Kept pure so the -o / -od / -odr mutual-exclusion and
// config-odr rules are unit-testable independently of main()'s exit path.
type outputMode struct {
	destDedup   bool // -od or -odr: incremental dedup against a library
	dryRun      bool // -odr: write nothing, just preview
	outArg      string
	outFlagName string // "-o" | "-od" | "-odr", for error messages
}

// resolveOutputMode enforces the -o / -od / -odr mutual exclusion and the
// config-odr-on-od rule. odPassed/odrPassed are whether the user set those
// flags on the CLI (flag.Visit); odrCfg is the resolved [sfu].odr bool.
func resolveOutputMode(out, outDedup, outDryRun string, odPassed, odrPassed, odrCfg bool) (outputMode, error) {
	outCount := 0
	if out != "" {
		outCount++
	}
	if outDedup != "" {
		outCount++
	}
	if outDryRun != "" {
		outCount++
	}
	if outCount > 1 {
		return outputMode{}, fmt.Errorf("-o, -od, and -odr are mutually exclusive; pick one")
	}
	if odPassed && strings.TrimSpace(outDedup) == "" {
		return outputMode{}, fmt.Errorf("-od requires a directory path; got empty string")
	}
	if odrPassed && strings.TrimSpace(outDryRun) == "" {
		return outputMode{}, fmt.Errorf("-odr requires a directory path; got empty string")
	}
	m := outputMode{outArg: out, outFlagName: "-o"}
	if outDedup != "" {
		m.destDedup = true
		m.outArg = outDedup
		m.outFlagName = "-od"
	}
	if outDryRun != "" {
		m.destDedup = true
		m.dryRun = true
		m.outArg = outDryRun
		m.outFlagName = "-odr"
	}
	// config odr=true flips dry-run on a -od run, reusing the od path.
	if !m.dryRun && odrCfg {
		if !m.destDedup {
			return outputMode{}, fmt.Errorf("[sfu] odr=true requires a library path; set [sfu].od or pass -od/-odr DIR")
		}
		m.dryRun = true
		m.outFlagName = "-odr"
	}
	return m, nil
}

// outputUnderInputTree reports whether outDir is the same as or nested inside
// the input tree, judged by filesystem identity rather than lexical path
// shape: both paths are canonicalized through their deepest existing ancestor
// (so symlinked parents resolve), the two directories are compared with
// os.SameFile for equality, and only then is filepath.Rel applied to the
// canonical paths. A path that cannot be statted or resolved (a symlink loop,
// an unreadable ancestor) fails closed with an error instead of being treated
// as outside.
func outputUnderInputTree(outDir, inputDir string) (bool, error) {
	if outDir == "" || inputDir == "" {
		return false, nil
	}
	// Identity check first: the same directory reached by two different paths
	// (alias, mount) is inside the tree regardless of spelling.
	if outInfo, err := os.Stat(outDir); err == nil {
		if inInfo, rerr := os.Stat(inputDir); rerr == nil && os.SameFile(outInfo, inInfo) {
			return true, nil
		}
	}
	canonicalOut, err := pathident.CanonicalProspective(outDir)
	if err != nil {
		return false, fmt.Errorf("resolve output directory %s: %w", outDir, err)
	}
	canonicalInput, err := pathident.CanonicalProspective(inputDir)
	if err != nil {
		return false, fmt.Errorf("resolve input directory %s: %w", inputDir, err)
	}
	if canonicalOut == canonicalInput {
		return true, nil
	}
	rel, err := filepath.Rel(canonicalInput, canonicalOut)
	if err != nil {
		return false, fmt.Errorf("relate output %s to input tree %s: %w", outDir, inputDir, err)
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// joutRef carries the -json stream into main's panic hook once it
// exists, so a mid-run panic still emits the error terminal before the
// crash, and into fatalf/usagef so os.Exit paths close the stream. It is set
// only after Start returned; before that it is nil and every exit is a
// no-op for the stream. Package scope because os.Exit funnels in fatalf and
// usagef are package-level.
var joutRef *jsonOut

// tuiStop tears down the live TUI monitor (alt-screen frame) before fatal or
// usage prose: prose written while the alt-screen is up vanishes when the
// registry restores the terminal at the exit seam. Set in main as soon as
// the monitor can be running (after Resolve, before the run); nil and no-op
// before that. Package scope because fatalf and usagef are.
var tuiStop func()

func main() {
	// A panic anywhere below (e.g. inside ulpengine.Run) would otherwise crash
	// with the alt-screen still up and the cursor hidden. Restore first, then
	// re-panic so the stack trace prints on a clean screen. No-op until the
	// monitor installs the hook.
	defer func() {
		if r := recover(); r != nil {
			reg.Restore()
			if joutRef != nil {
				joutRef.stop(tuistat.EventError, fmt.Sprintf("panic: %v", r))
			}
			panic(r)
		}
	}()

	// enable VT processing on Windows so TUI ANSI renders. no-op on Unix.
	// vtOK is false only on a legacy console that can't render ANSI, which
	// forces plain mode below so escapes never leak as raw text.
	vtOK := console.EnableVT()

	started := time.Now()
	runID, err := ulpengine.NewRunID()
	if err != nil {
		fatalf("%v", err)
	}
	stamp := ulpengine.RunStamp(started, runID)

	// flag.Usage is the trivial-error footer: a single hint line, not the
	// full help dump — the full help stays on explicit -h (printHelp). The
	// flag package also calls it after a parse error (ExitOnError), so
	// undefined or malformed flags stay short too.
	flag.Usage = func() { fmt.Fprintln(os.Stderr, "run with -h for help") }

	// resolve --version before loading cfg so bad cfg
	// doesnt block diagnostic output
	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastULP %s\n", version.String)
		return
	}

	// `update` / `upgrade`: replace installed SnowFast binaries with the latest release.
	// Handled before --help so `sfu update --help` reaches the update subcommand's
	// own help text instead of the generic top-level help. Also before cfg load so a
	// bad config can't block self-update.
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

	// Gate color on stderr (the TUI + summary target): a redirected stderr log
	// must never accumulate ANSI escapes even when stdout is a TTY.
	applyStderrColorProfile()

	config.EnsureMigrated(os.Args[1:], version.String, os.Stderr)
	fileCfg, err := config.LoadFromArgv(os.Args[1:])
	if err != nil {
		fatalf("%v", err)
	}

	out := flag.String("o", "", "output directory (default: CWD; see -h for file naming)")
	outDedup := flag.String("od", "", "output directory with incremental dedup against past sfu_*.txt.zst archives in the same dir (auto-enables -zst; mutually exclusive with -o)")
	outDryRun := flag.String("odr", "", "like -od but writes nothing: preview what a run would add to the library (auto-enables -zst; mutually exclusive with -o and -od)")
	workers := flag.Int("workers", 0, "phase-1 parser goroutines (0=auto)")
	workersAlias := flag.Int("j", 0, "alias for -workers")
	dedupW := flag.Int("dedup", 0, "phase-2 dedup goroutines (0=auto)")
	buckets := flag.Int("buckets", 0, "override adaptive bucket count (0=auto)")
	tempDir := flag.String("temp-dir", "", "directory for shard temp files (default: same dir as -o)")
	noTUI := flag.Bool("no-tui", false, "disable live TUI; print plain summary at end")
	zst := flag.Bool("zst", false, "compress output with zstd (highly efficient and searchable)")
	splitZst := flag.Int64("split-zst", ulpengine.DefaultZstChunkLines, "with -zst: split every N unique lines (default ~1.5GB/part); 0=single archive")
	delSrc := flag.Bool("del", false, "after success, delete all parsed input .txt files (irreversible)")
	noURI := flag.Bool("no-uri", false, "emit host:login:password (drop URL path/query)")
	loose := flag.Bool("loose", false, "high-recall parser: also accepts bare host/IP forms and other less precise shapes")
	parseDelims := flag.String("parse-delims", "", "replace built-in parser: split each line as url<SEP>login<SEP>password (exactly 3 fields; ignores -loose)")
	parseRules := flag.String("parse-rules", "", "replace built-in parser: file of regexps, one per line, with named groups url|host, login, password (ignores -loose)")
	noEncodingSniff := flag.Bool("no-encoding-sniff", false, "skip BOM detection; treat all inputs as UTF-8 (debug / A-B benchmark)")
	noFastPath := flag.Bool("no-fast-path", false, "disable the single-goroutine fast path (debugging)")
	debug := flag.Bool("debug", false, "write structured job debug log in current working directory (CWD at start)")
	debugReject := flag.Bool("debug-reject", false, "append parser-rejected lines to a file in CWD")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable background update availability check")
	bellOn := flag.Bool("bell", false, "play a short sound when the run finishes")
	historyOn := flag.Bool("history", false, "skip sources already completed in the shared history database")
	historyPath := flag.String("history-path", "", "shared history SQLite database path (does not enable history)")
	jsonOutFlag := &cliargs.OutTarget{}
	flag.Var(jsonOutFlag, "json", "stream live stats as one JSON object per line: bare = stdout, =FILE = file")
	jsonEvery := flag.Duration("json-every", defaultJSONEvery, "update interval for -json")

	// allow positional anywhere on cmdline. flag.Parse stops at first
	// non-flag, so split first and pass only flag tokens
	flagArgs, positional := cliargs.SplitPositional(config.StripConfigArgv(os.Args[1:]), flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		// CommandLine defaults to ExitOnError, this branch only fires
		// if mode is ever switched. treat as usage failure
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
	if err := fileCfg.ApplySFU(visited, config.SFUFlags{
		O: out, OD: outDedup, ODR: &odrCfg, TempDir: tempDir,
		Workers: workers, Dedup: dedupW, Buckets: buckets,
		SplitZst: splitZst,
		NoTUI:    noTUI, Zst: zst, Del: delSrc, NoURI: noURI,
		Loose: loose, NoEncodingSniff: noEncodingSniff,
		ParseDelims: parseDelims, ParseRules: parseRules,
		NoFastPath: noFastPath,
		Debug:      debug, DebugReject: debugReject,
		NoUpdateCheck: noUpdateCheck,
		Bell:          bellOn,
		JSONOut:       jsonOutFlag, JSONEvery: jsonEvery,
	}); err != nil {
		fatalf("%v", err)
	}
	bellEnabled = *bellOn

	var inputArg string
	switch len(positional) {
	case 0:
		// no CLI positional, try [sfu].input from config
		cfgInput, err := fileCfg.ResolvedSFUDir("input")
		if err != nil {
			fatalf("%v", err)
		}
		if cfgInput == "" {
			cfgHint := "set [sfu].input in your config or pass INPUT_PATH on the CLI"
			if fileCfg.Path() != "" {
				cfgHint = fmt.Sprintf("set [sfu].input in %s or pass INPUT_PATH on the CLI", fileCfg.Path())
			}
			fmt.Fprintf(os.Stderr, "sfu: no input path provided; %s\n", cfgHint)
			flag.Usage()
			reg.ExitWithCode(2)
		}
		inputArg = cfgInput
	case 1:
		inputArg = positional[0]
	default:
		fmt.Fprintf(os.Stderr, "sfu: expected exactly one input path; got %d\n", len(positional))
		flag.Usage()
		reg.ExitWithCode(2)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fatalf("getwd: %v", err)
	}

	inputs, err := ulpengine.CollectInputs(inputArg)
	if err != nil {
		// Exit-code policy (internal/exitcode): "no .txt files found under X"
		// is nothing-discovered, the same class sfl exits 4 for — not a hard
		// runtime error. Message text is unchanged; only the code moved 1 → 4.
		if strings.Contains(err.Error(), "no .txt files found under: ") {
			fmt.Fprintf(os.Stderr, "sfu: input: %v\n", err)
			reg.ExitWithCode(exitcode.NothingUsable)
		}
		fatalf("input: %v", err)
	}
	// The stream truncates its target before any input is read, so a target
	// that is (or lives under) an input would destroy data
	// (`sfu -json clob.txt clob.txt`). Refuse before anything is opened
	// or written.
	// A non-positive -json-every is an invocation error (M-11): validate it
	// here, on the usage path, so the later newJSONOut sites can treat every
	// remaining error as environmental (runtime exit 1).
	if jsonOutFlag.Enabled && *jsonEvery <= 0 {
		usagef("invalid -json-every %v: must be positive", *jsonEvery)
	}
	if err := rejectJSONOutInputCollision(jsonOutTarget(jsonOutFlag), inputArg, inputs, *parseRules); err != nil {
		usagef("%v", err)
	}
	// The stream also truncates its target before the history store opens,
	// so a target naming the effective history database or its WAL/SHM
	// sidecars would destroy the shared DB underneath the run. Resolve the
	// endpoints exactly as the store will and refuse before anything is
	// opened (the directory -history-path spelling resolves only at open).
	if *historyOn {
		if hit := history.JSONOutHistoryCollision(jsonOutTarget(jsonOutFlag), *historyPath); hit != "" {
			usagef("invalid -json target %q: overlaps history database %q", jsonOutTarget(jsonOutFlag), hit)
		}
	}

	// -o, -od, -odr mutually exclusive. -od does incremental dedup vs past
	// sfu archives and implies -zst (sidecar/regen only reads .zst).
	// -odr is -od's dry-run twin: same pipeline + stats, no library writes.
	// flag.Visit only iterates user-set flags, so explicit `-od ""` /
	// `-odr ""` show up while a missing flag doesnt
	odPassed := visited["od"]
	odrPassed := visited["odr"]
	mode, err := resolveOutputMode(*out, *outDedup, *outDryRun, odPassed, odrPassed, odrCfg)
	if err != nil {
		usagef("%v", err)
	}
	destDedup := mode.destDedup
	dryRun := mode.dryRun
	outArg := mode.outArg
	outFlagName := mode.outFlagName
	if destDedup && !*zst {
		*zst = true
	}

	// Validate custom parser configuration before history can take the all-hit
	// early return. Parser options are invocation errors regardless of whether
	// any source still needs processing.
	var (
		parser       ulpengine.LineParser
		parserDesc   string // TUI badge text, e.g. "delims '|'" or "rules (12)"
		looseIgnored bool
	)
	if *parseDelims != "" && *parseRules != "" {
		usagef("-parse-delims and -parse-rules are mutually exclusive")
	}
	if *parseDelims != "" {
		p, err := ulpengine.NewDelimParser(*parseDelims)
		if err != nil {
			usagef("parse delimiters: %v", err)
		}
		parser = p
		parserDesc = fmt.Sprintf("delims %q", *parseDelims)
	} else if *parseRules != "" {
		p, n, err := ulpengine.NewRegexRulesParser(*parseRules)
		if err != nil {
			usagef("parse rules: %v", err)
		}
		parser = p
		parserDesc = fmt.Sprintf("rules (%d)", n)
	}
	if parser != nil && *loose {
		// custom mode replaces the built-in parser wholesale; -loose has no
		// effect. Discreet notice (TUI badge + one stderr line), not an error.
		looseIgnored = true
		fmt.Fprintf(os.Stderr, "warning: -loose ignored (custom parser: %s)\n", parserDesc)
	}

	// Install signal handling before history hashing so a large preflight read is
	// cancellable just like the parser pipeline.
	ctx, cancel, signaled := reg.SignalContext()
	defer cancel()

	// Track open file handles so a graceful Ctrl-C can unstick reads blocked on
	// slow storage; WatchInterrupt force-exits if cleanup overruns the grace.
	files := &fileabort.Registry{}
	ctx = fileabort.WithContext(ctx, files)
	go reg.WatchInterrupt(ctx, files, signaled)

	var (
		historyStore     history.Store
		preparedHistory  *historyPreparation
		historyProgWrite io.Writer
	)
	var jout *jsonOut

	// TUI monitor wiring. The monitor starts after Resolve and the preflight
	// prompt, right before the run: the sampled history check renders on the
	// inline pre-pass bar instead, so the alt-screen session only ever spans
	// the run itself.
	tuiEng := &tuiEngine{}
	doneCh := make(chan struct{})
	// -no-tui hides the live screen only. -json hides it only when the
	// stream target is stdout and stdout is a terminal. The end summary still
	// prints on stderr. The config merge has already run, so this covers
	// json_out in the file too.
	tuiOff := !tuistat.ShowLiveDisplay(
		*noTUI,
		jsonOutTarget(jsonOutFlag),
		stderrIsCharDevice(),
		stdoutIsCharDevice(),
		vtOK,
	)
	var monitorWG sync.WaitGroup
	monitorRunning := false
	startMonitor := func() {
		if tuiOff || monitorRunning {
			return
		}
		monitorRunning = true
		monitorWG.Add(1)
		go monitor(doneCh, started, tuiEng, signaled, &monitorWG)
	}
	stopMonitor := func() {
		if !monitorRunning {
			return
		}
		monitorRunning = false
		close(doneCh)
		monitorWG.Wait()
		doneCh = make(chan struct{})
	}
	tuiStop = stopMonitor
	// Resolve and validate the output flag BEFORE the history prehash: the
	// all-skip short-circuit below returns early, and a bad -o (no dir hint,
	// no trailing separator, not an existing directory) must be a usage error
	// even when every source is already completed — same validate-first order
	// as sfl. ResolveDir is read-only; EnsureReady stays after the skip so
	// skipped runs still create no output directory.
	outDir, autoMkdir, err := outdir.ResolveDir(outFlagName, outArg)
	if err != nil {
		usagef("%v", err)
	}
	outDirAbs, err := filepath.Abs(outDir)
	if err != nil {
		fatalf("resolve output dir: %v", err)
	}
	if *historyOn {
		historyStore, err = openSFUHistory(*historyPath, dryRun)
		if err != nil {
			fatalf("open history: %v", err)
		}
		if historyStore != nil {
			defer historyStore.Close()
		}
		// The interactive pre-pass surface draws only when the TUI would
		// draw: stderr is a terminal and the run is neither -no-tui,
		// -json, nor a legacy console. Pipes, logs, and JSON-stream
		// runs print nothing at all — no \r junk. The checking pass renders
		// on the inline pre-pass bar below; historyProgWrite also carries
		// the validating pass after the run.
		if !tuiOff {
			historyProgWrite = os.Stderr
		}
		// One emitter for the whole run, opened before the fingerprinting so
		// the stream starts in the checking-history phase with live byte
		// progress. joutRef arms main's panic hook for the history phase too.
		jout, err = newJSONOut(jsonOutTarget(jsonOutFlag), *jsonEvery, reg)
		if err != nil {
			// Target collisions and the interval were already refused above
			// with the usage code; whatever remains here (the stream file
			// cannot be opened) is environmental — runtime exit 1.
			fatalf("%v", err)
		}
		hist := &historyCounters{}
		jout.hc = hist
		jout.start()
		joutRef = jout
		// The sampled prehash is sub-second, so it renders on the inline
		// pre-pass bar (like validating) while stderr is still free; the
		// alt-screen monitor starts after Resolve, once extraction is
		// imminent.
		checkBar := newHistoryBar(historyProgWrite, "checking")
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
						checkBar.Update(int(hist.checked.Load()), int(hist.filesTotal.Load()), hist.bytesDone.Load(), hist.bytesTotal.Load())
					}
				}
			}()
		}
		prepared, err := prepareHistory(ctx, historyStore, inputs, hist)
		if checkBar != nil {
			close(stopBar)
			barWG.Wait()
			checkBar.Finish()
		}
		if err != nil {
			// ctx.Err at prehash = user Ctrl-C'd. exit 130 not "prepare history: ..."
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "\ninterrupted")
				jout.stop(tuistat.EventInterrupted, "")
				reg.ExitWithCode(termctl.InterruptExitCode())
			}
			fatalf("prepare history: %v", err)
		}
		preparedHistory = &prepared
		if len(prepared.PendingPaths) == 0 {
			// History-only early return: every source is already completed.
			// The stream opened with the run, so it closes here with the
			// full start → updates → done → summary sequence; the summary's
			// history block carries the checked/skipped tallies.
			//
			// The live frame comes down before any prose so the all-hit line
			// lands on the restored screen, never under the alt-screen.
			stopMonitor()
			// Mirror the full-run -del flow: deletion here is irreversible,
			// so both success and failure must report exactly which sources
			// were removed and which remain.
			var intended []string
			if *delSrc && !dryRun {
				_, roots, planErr := eligibleDeleteCandidates(prepared.Candidates, nil)
				if planErr != nil {
					// fatalf funnels the stream stop itself; its formatted
					// message (with the "history: plan deletion" context) is
					// what rides the JSON terminal.
					fatalf("history: plan deletion: %v", planErr)
				}
				intended = roots
			}
			deleted, runErr := runHistoryOnly(ctx, prepared, dryRun, *delSrc, os.Stderr)
			if runErr != nil {
				// Ctrl-C during staged deletion: record deleted-so-far before
				// exiting 130. Surviving sources stay in their quarantine
				// containers and are recovered by the stale-container sweeper.
				if ctx.Err() != nil {
					renderInterruptedDeletionOutcome(os.Stderr, intended, deleted, runErr)
					fmt.Fprintln(os.Stderr, "\ninterrupted")
					jout.stop(tuistat.EventInterrupted, "")
					reg.ExitWithCode(termctl.InterruptExitCode())
				}
				// A partial deletion cannot be rolled back: report exactly
				// which sources are gone and which remain, like the full-run
				// -del path.
				outcome := summarizeDeletion(intended, deleted)
				for _, ln := range renderDeletionOutcome(outcome, runErr) {
					fmt.Fprintln(os.Stderr, ln)
				}
				jout.stopWithCode(tuistat.EventError, deletionJSONMessage(outcome, runErr), exitcode.Error)
				reg.ExitWithCode(1)
			}
			reported := dedupeSorted(deleted)
			if len(reported) > 0 {
				fmt.Fprintf(os.Stderr, "history: deleted %d source(s):\n", len(reported))
				for _, path := range reported {
					fmt.Fprintln(os.Stderr, "    "+path)
				}
			}
			jout.stop(tuistat.EventDone, "")
			// Mirror sfl: history-all-skip still finished a run, so -bell rings.
			if bellEnabled {
				bell.Ring()
			}
			return
		}
	}

	// RR-6.6: uncompressed output inside the scanned input tree is
	// rediscovered by the next -del run's CollectInputs and then deleted as a
	// "source". Reject the nesting up front — before any directory, output,
	// or temp file is created — instead of silently excluding sfu_*.txt files
	// (a user-owned input can legitimately carry that name).
	if *delSrc && !dryRun && !*zst {
		info, statErr := os.Stat(inputArg)
		if statErr == nil && info.IsDir() {
			under, err := outputUnderInputTree(outDirAbs, inputArg)
			if err != nil {
				usagef("%v", err)
			}
			if under {
				usagef("-del with uncompressed output cannot nest the output directory inside the input tree: %s is the same as or inside %s; a rerun would collect and delete the previous sfu_*.txt output. Use -zst (compressed output is not rescanned) or place the output outside the input tree.", outDirAbs, inputArg)
			}
		}
	}
	base := ulpengine.DefaultBasename(stamp)

	// output path = dir + basename + optional .zst. 1 vs N parts decided
	// later by chunkedZstdSink. multi-part rename only fires when _part2
	// actually opens, so single-archive runs never carry _part suffix
	absOut, err := filepath.Abs(filepath.Join(outDirAbs, ulpengine.WithZstExt(base, *zst)))
	if err != nil {
		fatalf("resolve output: %v", err)
	}

	if autoMkdir {
		if err := outdir.EnsureReady(outFlagName, outDirAbs, !dryRun); err != nil {
			fatalf("%v", err)
		}
	}

	effectiveInputs := inputs
	if preparedHistory != nil {
		effectiveInputs = preparedHistory.PendingPaths
	}
	effectiveTempDir := *tempDir
	if dryRun && strings.TrimSpace(effectiveTempDir) == "" {
		dryRunTempDir, err := os.MkdirTemp("", "sfu-odr-")
		if err != nil {
			fatalf("create dry-run temp dir: %v", err)
		}
		defer os.RemoveAll(dryRunTempDir)
		effectiveTempDir = dryRunTempDir
	}
	cfg := ulpengine.Config{
		Inputs:          effectiveInputs,
		Output:          absOut,
		TempDir:         effectiveTempDir,
		Workers:         *workers,
		DedupWorkers:    *dedupW,
		Buckets:         *buckets,
		FastPathOff:     *noFastPath,
		Compress:        *zst,
		ZstChunkLines:   *splitZst,
		RunStarted:      started,
		RunStamp:        stamp,
		DeleteInputs:    *delSrc,
		NoURI:           *noURI,
		Loose:           *loose,
		Parser:          parser,
		NoEncodingSniff: *noEncodingSniff,
		DestDedup:       destDedup,
		DestDedupDir:    outDirAbs,
		DryRun:          dryRun,
	}
	r, err := ulpengine.Resolve(cfg)
	if err != nil {
		fatalf("config: %v", err)
	}
	ulpengine.EnsureDestDedupMetrics(r)
	if preparedHistory != nil {
		r.HistoryChecked = len(preparedHistory.Candidates)
		r.HistorySkipped = len(preparedHistory.Hits)
	}
	// custom-parser metadata for the TUI header badges.
	r.ParserDesc = parserDesc
	r.LooseIgnored = looseIgnored

	var dbg *ulpengine.DebugLog
	var rr *ulpengine.RejectRecorder
	var debugLogPath string
	if *debug {
		f, p, err := ulpengine.CreateArtifactFile(cwd, "sfu-debug-"+stamp, ".log", 0o600)
		if err != nil {
			fatalf("debug log: %v", err)
		}
		debugLogPath = p
		dbg = ulpengine.NewDebugLogFile(f)
		r.Cfg.Debug = dbg
	}
	if *debugReject {
		f, _, err := ulpengine.CreateArtifactFile(cwd, "sfu-rejected-"+stamp, ".txt", 0o644)
		if err != nil {
			fatalf("debug-reject: %v", err)
		}
		rr = ulpengine.NewRejectRecorderFile(f)
		r.Cfg.Reject = rr
	}
	closeDebugArtifacts := func() {
		if dbg != nil {
			_ = dbg.Close()
		}
		if rr != nil {
			_ = rr.Close()
		}
	}
	defer closeDebugArtifacts()
	// ExitWithCode (fatal, usage, and every interrupt/abort seam) and
	// ForceExit (second Ctrl-C, cleanup timeout) end in os.Exit, which skips
	// the deferred closeDebugArtifacts. Arm the registry's pre-exit flush so
	// buffered -debug / -debug-reject lines reach the files on every abnormal
	// exit — the reject recorder in particular is never flushed mid-run, so
	// an interrupt or late fatal would otherwise lose everything it recorded.
	// The flush is silent and best-effort by contract.
	reg.SetExitFlush(func() {
		if dbg != nil {
			dbg.Flush()
		}
		if rr != nil {
			rr.Flush()
		}
	})

	binName := filepath.Base(os.Args[0])
	if dbg != nil {
		dbg.WriteHeader(binName, started, os.Args, inputs, r)
		dbg.LogResolutionRationale(r)
		if *debug {
			fmt.Fprintf(os.Stderr, "debug log: %s\n", debugLogPath)
		}
	}
	// -split-zst-without-zst warning unconditional so non-debug users see it too
	if visited["split-zst"] && !*zst {
		fmt.Fprintf(os.Stderr, "warning: -split-zst %d ignored without -zst\n", *splitZst)
		if dbg != nil {
			dbg.Event("warn: -split-zst %d ignored without -zst", *splitZst)
		}
	}

	updateChecker := selfupdate.NewChecker(version.String, os.Args[0], *noUpdateCheck)
	updateChecker.Start()

	// sweep orphan shard subdirs from crashed runs. best-effort,
	// failures silent in sweepStaleTempDirs
	if err := os.MkdirAll(r.TempDir, 0o755); err == nil {
		if n := ulpengine.SweepStaleWorkDirs(r.TempDir, ""); n > 0 {
			dbg.Event("swept %d orphan temp dir(s) under %s", n, r.TempDir)
		}
	}

	// Signal handling was installed before history hashing; it also covers the
	// preflight prompt so Ctrl-C exits 130 instead of being swallowed.
	ok, err := preflightCheck(ctx, r, isStdinTTY(os.Stdin), os.Stdin, os.Stderr)
	if err != nil {
		// ctx.Err at the prompt = user Ctrl-C'd. exit 130 not "preflight: ..."
		if ctx.Err() != nil {
			// the prompt is plain stderr prose (the monitor is not up yet)
			fmt.Fprintln(os.Stderr, "\ninterrupted")
			if jout != nil {
				jout.stop(tuistat.EventInterrupted, "")
			}
			reg.ExitWithCode(termctl.InterruptExitCode())
		}
		fatalf("preflight: %v", err)
	}
	if !ok {
		// The prompt's verdict is plain stderr prose (the monitor is not up
		// yet).
		fmt.Fprintln(os.Stderr, "aborted by user")
		if jout != nil {
			jout.stopWithCode(tuistat.EventError, "aborted by user", exitcode.Usage)
		}
		reg.ExitWithCode(2)
	}

	m := &ulpengine.Metrics{TotalInputBytes: r.TotalInputs}

	if jout == nil {
		// No history phase: the stream opens here, exactly where it did
		// before the checking-history coverage was added. The engine is
		// handed over before start: start builds the first snapshot
		// synchronously, and with the engine unresolved it would mislabel
		// a history-less run's first line as checking-history.
		jout, err = newJSONOut(jsonOutTarget(jsonOutFlag), *jsonEvery, reg)
		if err != nil {
			// See the history-phase site: only environmental failures remain
			// here — runtime exit 1.
			fatalf("%v", err)
		}
		jout.setEngine(m, r)
		jout.start()
	} else {
		// The stream opened before the engine existed (history phase); hand
		// it over so the next tick renders the engine phases.
		jout.setEngine(m, r)
	}
	joutRef = jout
	// Normal completion ends the stream in "done"; the exit paths below that
	// call reg.ExitWithCode (os.Exit, which skips defers) stop it explicitly.

	// Hand the monitor the engine objects and start it: with -history the
	// check already rendered on the inline pre-pass bar, so the alt-screen
	// session begins here and spans only the run.
	tuiEng.setEngine(m, r)
	startMonitor()

	runErr := ulpengine.Run(ctx, r, m)

	close(doneCh)

	// wait for monitor's deferred frame.close to leave alt-screen
	// cleanly before printing summary. replaces a race-y 50ms sleep
	if monitorRunning {
		monitorWG.Wait()
	}

	if runErr != nil {
		// user Ctrl-C = exit 130 + terse msg, not "context canceled"
		sig := signaled()
		if dbg != nil {
			dbg.LogTermination(runErr, sig, time.Since(started))
		}
		if sig {
			// reassure a confused user who Ctrl+C'd mid-migration: the dest
			// library is only ever touched via atomic sidecar renames + a
			// discarded-on-failure output, so nothing is half-written.
			if r.Cfg.DestDedup {
				fmt.Fprintln(os.Stderr, "\ninterrupted — existing library left intact (no archives modified); safe to re-run.")
			} else {
				fmt.Fprintln(os.Stderr, "\ninterrupted")
			}
			jout.stop(tuistat.EventInterrupted, "")
			ulpengine.PrintManualCleanupHint(os.Stderr)
			reg.ExitWithCode(termctl.InterruptExitCode())
		}
		jout.stopWithCode(tuistat.EventError, runErr.Error(), exitcode.Error)
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", runErr)
		reg.ExitWithCode(1)
	}

	// nothingParsed must be classified BEFORE history commit or deletion:
	// an all-reject run produced nothing usable (exit 4 below), so recording
	// the sources as completed or deleting them would destroy the only
	// evidence while the command reports failure. The counters are final
	// once Run returned.
	nothingParsed := m.LinesAccepted.Load() == 0 && m.LinesRejected.Load() > 0

	var historyCommitErr error
	if preparedHistory != nil && !nothingParsed {
		if err := commitHistory(ctx, historyStore, *preparedHistory, dryRun, historyProgWrite, r.OutputPaths...); err != nil {
			// Ctrl-C during the validating-before-record pass: exit 130 like
			// every other signal path, inputs were not deleted.
			if ctx.Err() != nil {
				jout.stop(tuistat.EventInterrupted, "")
				fmt.Fprintln(os.Stderr, "\ninterrupted")
				reg.ExitWithCode(termctl.InterruptExitCode())
			}
			// Same recoverable state as sfl's history seams: the output is
			// committed and the inputs were not deleted, so the run still ends
			// with its summary — the failed history write is surfaced inside
			// and after it instead of replacing it.
			historyCommitErr = err
			dbg.Event("history: commit failed: %v", err)
		}
	}

	if !nothingParsed && historyCommitErr == nil && *delSrc && !dryRun {
		// delete BEFORE logCompletion so outcome lands in same block
		var intended, deleted []string
		if preparedHistory != nil {
			eligible, roots, planErr := eligibleDeleteCandidates(preparedHistory.Candidates, r.OutputPaths)
			if planErr != nil {
				// fatalf funnels the stream stop itself; its formatted
				// message (with the "history: plan deletion" context) is
				// what rides the JSON terminal.
				fatalf("history: plan deletion: %v", planErr)
			}
			intended = roots
			deleted, err = history.DeleteStaged(ctx, roots, eligible, afterHistoryDeleteStage)
		} else {
			intended, err = intendedDeletionRoots(inputs, r.OutputPaths)
			if err != nil {
				// fatalf funnels the stream stop itself; its formatted
				// message (with the "delete inputs" context) is what rides
				// the JSON terminal.
				fatalf("delete inputs: %v", err)
			}
			deleted, err = ulpengine.DeleteParsedInputs(inputs, r.OutputPaths)
		}
		if err != nil {
			// Ctrl-C during staged deletion: report deleted-so-far before
			// exiting 130. Surviving sources stay in their quarantine
			// containers and are recovered by the stale-container sweeper.
			// The JSON terminal stays EventInterrupted exactly once — the
			// prose report must not alter the JSON contract.
			if ctx.Err() != nil {
				renderInterruptedDeletionOutcome(os.Stderr, intended, deleted, err)
				jout.stop(tuistat.EventInterrupted, "")
				fmt.Fprintln(os.Stderr, "\ninterrupted")
				reg.ExitWithCode(termctl.InterruptExitCode())
			}
			// A partial deletion is irreversible: report exactly which
			// sources are gone and which remain — on stderr and in the JSON
			// error terminal — never claiming a rollback.
			outcome := summarizeDeletion(intended, deleted)
			jout.stopWithCode(tuistat.EventError, deletionJSONMessage(outcome, err), exitcode.Error)
			dbg.Event("del: FAILED after removing %d/%d input(s) err=%v", len(deleted), len(intended), err)
			for _, ln := range renderDeletionOutcome(outcome, err) {
				fmt.Fprintln(os.Stderr, ln)
			}
			reg.ExitWithCode(1)
		}
		r.DeletedInputPaths = deleted
		dbg.Event("del: removed %d input file(s)", len(deleted))
	}

	if dbg != nil {
		dbg.LogCompletion(m, time.Since(started), r)
	}

	// DONE block to stderr, alt-screen already left. stderr keeps stdout
	// clean for `sfu in -o ./out/ | grep ...` pipelines. lipgloss strips
	// styling automatically on non-TTY stderr
	// Exit-code policy (internal/exitcode): sfu has no per-source failure
	// model — line-level rejects are normal noise while some lines parse. A
	// run that read lines but parsed none is a total failure (exit 4), so the
	// stream ends in error and the process exits 4 after the summary.
	// nothingParsed was classified above, before history commit or deletion.
	if nothingParsed {
		jout.stopWithCode(tuistat.EventError, "no line parsed: every read line was rejected", exitcode.NothingUsable)
	} else if historyCommitErr != nil {
		// The failed history write rides the JSON terminal as the error event;
		// the human summary below still renders.
		jout.stopWithCode(tuistat.EventError, fmt.Sprintf("output committed, but history commit failed; inputs were not deleted: %v", historyCommitErr), exitcode.Error)
	} else {
		jout.stop(tuistat.EventDone, "")
	}
	tw := termWidth()
	// NoticeForSummary returns nil when the check is disabled, so no extra guard.
	updateNotice := updateChecker.NoticeForSummary()
	for _, ln := range renderFinalStdoutSummary(time.Since(started), m, r, tw, updateNotice) {
		fmt.Fprintln(os.Stderr, ln)
	}
	if bellEnabled {
		bell.Ring()
	}
	if nothingParsed {
		// ExitWithCode os.Exits and skips the deferred artifact close, so
		// flush the -debug / -debug-reject buffers here: an all-rejects run
		// is exactly the case -debug-reject exists to diagnose, and skipping
		// the flush leaves the reject file empty.
		closeDebugArtifacts()
		reg.ExitWithCode(exitcode.NothingUsable)
	}
	if historyCommitErr != nil {
		// The recoverable state must reach the user: one warn line inside the
		// tail of the summary, then the full prose (same message the JSON
		// error terminal carries). sfu has no partial exit class, so the
		// history-write I/O failure stays a runtime error (1).
		fmt.Fprintln(os.Stderr, warnStyle.Render("⚠ History not recorded")+
			mutedStyle.Render(" — output committed, inputs were not deleted"))
		fmt.Fprintf(os.Stderr, "\nsfu: output committed, but history commit failed; inputs were not deleted: %v\n", historyCommitErr)
		closeDebugArtifacts()
		reg.ExitWithCode(exitcode.Error)
	}
}

func fatalf(format string, args ...any) {
	// Tear the live frame down first so the prose lands on the restored
	// screen; a no-op until the monitor can be running.
	if tuiStop != nil {
		tuiStop()
	}
	// ExitWithCode is os.Exit: defers never run, so every fatal exit after
	// the stream opened (joutRef is set once start returned) must close it
	// with an error terminal first. Before that point joutRef is nil and
	// this is a no-op — the stream has no lines to close. This funnel is
	// the single stop for fatal paths: callers must not stop the stream
	// beforehand, or the formatted message here is silently dropped by the
	// emitter's once-guard and the raw error rides the wire instead.
	if joutRef != nil {
		joutRef.stopWithCode(tuistat.EventError, fmt.Sprintf(format, args...), exitcode.Error)
	}
	fmt.Fprintf(os.Stderr, "sfu: "+format+"\n", args...)
	reg.ExitWithCode(1)
}

// rejectJSONOutInputCollision refuses a -json target that is, or lives
// under, an input: the stream truncates its target before any input is read,
// so `sfu -json clob.txt clob.txt` (or the =FILE form, or a config input)
// would destroy the input. No filesystem writes; runs before the stream and
// every input open. inputArg is checked in addition to the collected inputs
// so a target inside an input directory is refused even when no collected
// file equals it. parseRules is the -parse-rules file: the stream would
// truncate it right after the parser read it. A target whose final component
// is a (possibly dangling) symlink is compared through its referent chain —
// the stream's create follows the link onto whatever it points at.
func rejectJSONOutInputCollision(target, inputArg string, inputs []string, parseRules string) error {
	if target == "" || target == "-" {
		return nil
	}
	tgt, err := pathident.CanonicalProspective(target)
	if err != nil {
		return fmt.Errorf("invalid -json target %q: %w", target, err)
	}
	for _, p := range append([]string{inputArg}, inputs...) {
		if inC, err := pathident.CanonicalProspective(p); err == nil && inC == tgt {
			return fmt.Errorf("invalid -json target %q: overlaps input %q", target, p)
		}
		if same, err := pathident.SameFile(tgt, p); err == nil && same {
			return fmt.Errorf("invalid -json target %q: overlaps input %q", target, p)
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			if dir, err := pathident.CanonicalProspective(p); err == nil && pathident.WithinDir(tgt, dir) {
				return fmt.Errorf("invalid -json target %q: overlaps input directory %q", target, p)
			}
		}
		// A symlink target dodges the compares above when it dangles: follow
		// the referent chain against the input (and, for directories, check
		// whether the referent lands inside the scanned tree).
		if pathident.LinkRefersTo(target, p) {
			return fmt.Errorf("invalid -json target %q: overlaps input %q", target, p)
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			if dir, derr := pathident.CanonicalProspective(p); derr == nil {
				if refs, lerr := pathident.LinkResolution(target); lerr == nil {
					for _, r := range refs {
						if pathident.WithinDir(r, dir) {
							return fmt.Errorf("invalid -json target %q: overlaps input directory %q", target, p)
						}
					}
				}
			}
		}
	}
	// The -parse-rules file is an input too: the stream opens after the
	// parser has read the rules, so a colliding target would truncate them
	// under the run.
	if parseRules != "" {
		if inC, err := pathident.CanonicalProspective(parseRules); err == nil && inC == tgt {
			return fmt.Errorf("invalid -json target %q: overlaps parse rules file %q", target, parseRules)
		}
		if same, err := pathident.SameFile(tgt, parseRules); err == nil && same {
			return fmt.Errorf("invalid -json target %q: overlaps parse rules file %q", target, parseRules)
		}
		if pathident.LinkRefersTo(target, parseRules) {
			return fmt.Errorf("invalid -json target %q: overlaps parse rules file %q", target, parseRules)
		}
	}
	return nil
}

// argv-shape error, exit 2 (distinct from runtime 1)
func usagef(format string, args ...any) {
	if tuiStop != nil {
		tuiStop()
	}
	if joutRef != nil {
		joutRef.stopWithCode(tuistat.EventError, fmt.Sprintf(format, args...), exitcode.Usage)
	}
	fmt.Fprintf(os.Stderr, "sfu: "+format+"\n", args...)
	flag.Usage()
	reg.ExitWithCode(2)
}

// tuiEngine carries the TUI monitor's data sources: the engine
// metrics/resolved pair main hands over after Resolve, before the monitor
// starts.
type tuiEngine struct {
	mPtr atomic.Pointer[ulpengine.Metrics]
	rPtr atomic.Pointer[ulpengine.Resolved]
}

// setEngine hands the monitor the resolved engine objects; the monitor is
// started right after, so every tick renders the normal phase frames.
func (t *tuiEngine) setEngine(m *ulpengine.Metrics, r *ulpengine.Resolved) {
	t.mPtr.Store(m)
	t.rPtr.Store(r)
}

// live status loop. samples metrics every ~300ms, computes per-tick
// rates, draws an 80-col block. signaled=true swaps to INTERRUPTED
// frame. wg.Done fires after frame.close so callers sync on clean exit.
// the monitor starts after Resolve (the history check renders on the
// inline pre-pass bar), so every tick renders the normal phase frames
func monitor(done <-chan struct{}, started time.Time, eng *tuiEngine, signaled func() bool, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}
	frame := tuiFrame{tty: stderrIsCharDevice()}
	// Route teardown through the frame's mutex-guarded close so the force-exit
	// goroutine never races the monitor's draw on stderr.
	reg.Set(frame.close)
	defer reg.Clear()
	defer frame.close()

	winch := make(chan os.Signal, 1)
	notifyTerminalResize(winch)
	defer signal.Stop(winch)

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	var prevCPU time.Duration
	var prevTime time.Time

	var prevAt time.Time
	var prevNormPhase int32 = -2 // sentinel, no prior sample
	var prevRead, prevShard, prevWritten int64
	// separate prev-state for OD regen bytes so the OD frame's
	// rate row ticks every redraw regardless of main frame state
	var prevRegenAt time.Time
	var prevRegenBytes int64

	draw := func() {
		now := time.Now()
		elapsed := now.Sub(started)
		w := termWidth()

		// interrupt overrides phase layout for the rest of process
		// lifetime. prev* not updated, rates meaningless in shutdown
		if signaled != nil && signaled() {
			m, r := eng.mPtr.Load(), eng.rPtr.Load()
			frame.draw(renderInterruptLines(elapsed, now, m, r, w, ulpengine.SnapshotCleanupLog()))
			return
		}

		m, r := eng.mPtr.Load(), eng.rPtr.Load()
		if m == nil || r == nil {
			// No engine yet: the monitor only starts after setEngine, so
			// this is a stray early tick — draw nothing.
			return
		}

		phase := m.Phase.Load()
		// phaseInit + phaseShard render the same PARSING panel, treat
		// as one phase for delta math. phasePhase0 keeps own bucket
		// (OD-specific rates shouldnt bleed into shard panel)
		normPhase := phase
		if phase == ulpengine.PhaseInit {
			normPhase = ulpengine.PhaseShard
		}

		read := m.BytesRead.Load()
		sh := m.BytesShard.Load()
		wr := m.BytesWritten.Load()

		var readBPS, shardBPS, writeBPS float64
		if !prevAt.IsZero() && normPhase == prevNormPhase {
			dt := now.Sub(prevAt).Seconds()
			if dt >= 0.05 {
				readBPS = float64(read-prevRead) / dt
				shardBPS = float64(sh-prevShard) / dt
				writeBPS = float64(wr-prevWritten) / dt
			}
		}

		// OD-frame throughput. computed unconditionally so phase 1/2
		// see a 0-rate snapshot of the (frozen) phase-0 counter
		var regenBPS float64
		if r.OdMetrics != nil {
			cur := r.OdMetrics.RegenBytesRead.Load()
			if !prevRegenAt.IsZero() {
				dt := now.Sub(prevRegenAt).Seconds()
				if dt >= 0.05 {
					regenBPS = float64(cur-prevRegenBytes) / dt
				}
			}
			prevRegenAt = now
			prevRegenBytes = cur
		}

		ramMB := float64(currentRSSBytes()) / (1024 * 1024)
		cpuPct := cpuPercent(&prevCPU, &prevTime)

		var lines []string
		switch phase {
		case ulpengine.PhasePhase0:
			// phase 0 has all shard counters at zero. surface just
			// the OD frame as primary so user sees discovery/regen
			// progress instead of a frozen 0% bar
			lines = renderPhase0Lines(elapsed, m, r, ramMB, cpuPct, regenBPS, w)
		case ulpengine.PhaseInit, ulpengine.PhaseShard:
			lines = renderShardLines(now, elapsed, m, r, ramMB, cpuPct, readBPS, shardBPS, regenBPS, w)
		case ulpengine.PhaseDedup:
			lines = renderDedupLines(now, elapsed, m, r, ramMB, cpuPct, writeBPS, regenBPS, w)
		case ulpengine.PhaseDone:
			// DONE is drawn to regular screen in main after alt-screen
			// leave so it sticks in scrollback. drawing here would
			// cause a brief flash on exit. return early lets deferred
			// frame.close run w/ dedup-100% frame showing
			return
		}
		frame.draw(lines)

		prevAt = now
		prevRead = read
		prevShard = sh
		prevNormPhase = normPhase
	}

	for {
		select {
		case <-done:
			draw()
			return
		case <-winch:
			frame.redrawOnResize()
		case <-ticker.C:
			draw()
		}
	}
}

// process CPU% between samples. 100% = one fully-used core.
// per-platform CPU time sourcing in procstats_{unix,windows}.go
func cpuPercent(prevCPU *time.Duration, prevTime *time.Time) float64 {
	now := time.Now()
	procCPU := processCPUTime()
	if prevTime.IsZero() {
		*prevCPU = procCPU
		*prevTime = now
		return 0
	}
	dCPU := procCPU - *prevCPU
	dTime := now.Sub(*prevTime)
	*prevCPU = procCPU
	*prevTime = now
	if dTime <= 0 {
		return 0
	}
	return 100 * float64(dCPU) / float64(dTime)
}
