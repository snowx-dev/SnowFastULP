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
	"sync"
	"sync/atomic"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/bell"
	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/console"
	"github.com/snowx-dev/SnowFastULP/internal/discover"
	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	"github.com/snowx-dev/SnowFastULP/internal/fdlimit"
	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// reg is the shared terminal restore/exit registry for the process: the live
// screen registers its teardown via Set, and every exit path (graceful
// ExitWithCode, force-exit on a second Ctrl-C, cleanup timeout) routes
// through it so the alt-screen is always left cleanly. nil cleanupHint: sfs
// has no manual-cleanup hint (unlike sfu/sfl).
var reg = termctl.New(os.Stderr, nil)

// joutRef carries the -json stream into main's panic hook once it
// exists, so a mid-run panic still emits the error terminal before the
// crash re-surfaces. Assigned only after start returned; nil otherwise (so
// every fatal/usage path before the stream opens skips the stop).
var joutRef *jsonOut

var bellEnabled bool

// restoreOnPanic is the terminal panic guard: on panic, restore the registered
// terminal hook (leave the alt screen, show the cursor), then re-panic so the
// crash still surfaces with its stack trace. main installs it as the outer
// defer, and the TUI goroutine wrapper uses it so a render/teardown panic
// restores before the goroutine dies. Never swallows the panic.
func restoreOnPanic() {
	if r := recover(); r != nil {
		reg.Restore()
		panic(r)
	}
}

func main() {
	// A panic anywhere below (e.g. in run while the TUI goroutine owns the
	// alt-screen) would otherwise crash with the screen still alternate and
	// the cursor hidden. Restore first, then re-panic so the stack trace
	// prints on a clean screen. A -json stream that opened past start
	// (joutRef) closes with an error terminal first; its once-guard makes a
	// later funnel/hook stop a harmless no-op. No-op restore until runUI
	// installs the hook.
	defer func() {
		if r := recover(); r != nil {
			reg.Restore()
			if joutRef != nil {
				joutRef.stop(tuistat.EventError, fmt.Sprintf("panic: %v", r))
			}
			panic(r)
		}
	}()

	// VT on for windows ANSI, no-op on unix. must run pre-output. vtOK is false
	// only on a legacy console that can't render ANSI, forcing the silent UI so
	// escapes never leak as raw text.
	vtOK := console.EnableVT()
	// started is the run timestamp, shared by -f per-pattern file naming and
	// the stats/debug headers; declared up front so the -f resolution block can
	// stamp the per-pattern output filenames.
	started := time.Now()

	// flag.Usage is the trivial-error footer: a single hint line, not the
	// full help dump — the full help stays on explicit -h (printHelp). The
	// flag package also calls it after a parse error (ExitOnError), so
	// undefined or malformed flags stay short too.
	flag.Usage = func() { fmt.Fprintln(os.Stderr, "run with -h for help") }

	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastSearch %s\n", version.String)
		return
	}

	// `update` / `upgrade`: replace installed SnowFast binaries with the latest release.
	// Handled before --help so `sfs update --help` reaches the update subcommand's
	// own help text instead of the generic top-level help. Also before cfg load so a
	// bad config can't block self-update.
	if handled, err := selfupdate.Dispatch(os.Args[1:], version.String, os.Stdout); handled {
		if err != nil {
			fatal("%v", err)
		}
		return
	}

	if cliargs.IsHelpRequest(os.Args[1:]) {
		printHelp(filepath.Base(os.Args[0]), os.Stdout)
		reg.ExitWithCode(0)
	}

	// Gate color on stderr (the live-screen target): a redirected stderr must
	// never accumulate ANSI escapes even when stdout is a TTY.
	applyStderrColorProfile()

	config.EnsureMigrated(os.Args[1:], version.String, os.Stderr)
	cfg, err := config.LoadFromArgv(os.Args[1:])
	if err != nil {
		fatal("%v", err)
	}

	outFile := flag.String("o", "", "also write results to this file (with -stats: file only; default stream: tee to stdout)")
	stats := flag.Bool("stats", false, "live progress screen; write hits to an auto result file (or -o)")
	// retained for parse-compat; stream is the default now so -s/-silent are no-ops.
	flag.Bool("s", false, "deprecated alias for default stream-to-stdout mode")
	txtMode := flag.Bool("txt", false, "search plain .txt files instead of .zst archives (no index)")
	flag.Bool("silent", false, "deprecated alias for -s")
	clean := flag.Bool("clean", false, "strip URL scheme prefixes from output lines")
	combo := flag.Bool("combo", false, "write only login:password per hit (L:P parsed from each result line); lines that are not U:L:P are skipped")
	since := flag.String("since", "", "only search archives modified within this window, e.g. 7d, 12h, 90m (default: all)")
	workers := flag.Int("j", 0, "")
	workersAlias := flag.Int("workers", 0, "") // sfu/sfl spelling
	debugFlag := flag.Bool("debug", false, "write structured job debug log in current working directory (CWD at start)")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable background update availability check")
	bellOn := flag.Bool("bell", false, "play a short sound when the run finishes")
	// 1 MiB default matches the search engine default; tune only after profiling.
	// zst-only: -txt reads use a fixed 1 MiB step and ignore this flag.
	decodeStep := flag.Int("decode-step", 0, "zst only: bytes per decoder read (0 = 1 MiB default; ignored in -txt mode)")
	// per-chunk safety valve vs pathological queries (eg `:` over multi-GiB).
	// 0 = unbounded, hit = skip rest of chunk + stderr note
	maxHitsPerChunk := flag.Int("max-hits-per-chunk", 0, "")
	// global hit cap: stop the whole search + exit cleanly after N total hits.
	// 0 = unlimited. distinct from -max-hits-per-chunk (per-chunk safety valve).
	limit := flag.Int("l", 0, "stop after N total hits, then exit (0 = unlimited)")
	patternsFile := flag.String("f", "", "file of search terms (one per line); with -o DIR, write one file per term")
	// -json streams live stats as NDJSON (mirror sfu/sfl): bare = stdout,
	// =FILE = a file. When enabled it OWNS stdout — hits require -o FILE (or
	// [sfs] o in config) and are routed there file-only. It hides the -stats
	// screen only when that stdout target is a terminal.
	jsonOutFlag := &cliargs.OutTarget{}
	flag.Var(jsonOutFlag, "json", "stream live stats as one JSON object per line: bare = stdout, =FILE = file. Hits require -o FILE (or [sfs] o). A stdout target on a terminal hides the -stats screen")
	jsonEvery := flag.Duration("json-every", defaultJSONEvery, "update interval for -json")

	flagArgs, positional := cliargs.SplitPositional(config.StripConfigArgv(os.Args[1:]), flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		reg.ExitWithCode(2)
	}
	visited := config.NewVisited()
	visited.ResolveIntAlias(workers, workersAlias, "j", "workers")
	if err := cfg.ApplySFS(visited, config.SFSFlags{
		O: outFile, Txt: txtMode, Stats: stats, Clean: clean, Combo: combo, J: workers, Debug: debugFlag,
		NoUpdateCheck: noUpdateCheck,
		Bell:          bellOn,
		DecodeStep:    decodeStep, MaxHitsPerChunk: maxHitsPerChunk, Limit: limit, Since: since,
		JSONOut: jsonOutFlag, JSONEvery: jsonEvery,
	}); err != nil {
		fatal("%v", err)
	}
	bellEnabled = *bellOn
	// -json needs an explicit hit destination, resolved right after the
	// config merge so [sfs] o counts. Captured here because -f -o DIR clears
	// *outFile below (the per-term files take over); the usage() refusal
	// itself fires later, after the -f-specific check, so -f mode keeps its
	// own message.
	jsonOutDestGiven := jsonOutFlag.Enabled && *outFile != ""

	w := *workers
	if w <= 0 {
		w = runtime.GOMAXPROCS(0)
	}

	fMode := *patternsFile != ""
	if fMode {
		var statsErr error
		*stats, statsErr = resolveFModeStats(*stats, visited["stats"])
		if statsErr != nil {
			usage("-f is not supported with -stats; use -o DIR for per-term files")
		}
		// -json owns stdout, so -f mode needs -o DIR for per-term files;
		// otherwise hits would have nowhere to go.
		if jsonOutFlag.Enabled && *outFile == "" {
			usage("-f with -json requires -o DIR for per-term files")
		}
	}
	args, err := parseSearchArgsMode(positional, fMode)
	if err != nil {
		usage("%v", err)
	}
	if fMode {
		var sfsDir string
		if args.Root == "" && cfg.SFS.Dir != "" {
			dir, derr := cfg.ResolvedSFSDir()
			if derr != nil {
				fatal("%v", derr)
			}
			sfsDir = dir
		}
		args.Root = applyFModeRoot(args.Root, sfsDir)
	} else if len(positional) == 1 {
		// PATTERN-only form: the search root comes from [sfs].dir. With none
		// configured, name the remedy instead of silently scanning CWD —
		// the headline `sfs PATTERN` usage depends on that config key.
		if cfg.SFS.Dir == "" {
			usage("no search directory configured; set [sfs].dir in your config or pass DIR PATTERN")
		}
		dir, err := cfg.ResolvedSFSDir()
		if err != nil {
			fatal("%v", err)
		}
		args.Root = dir
	}

	// Resolve the effective pattern set: a single CLI PATTERN, or the terms
	// loaded from -f. matcher stays nil for single-pattern (BMH hot path).
	var (
		patterns   []string
		matcher    *search.MultiMatcher
		multiSink  *dispatchSink
		multiPaths []string
	)
	patternFDCount := 0

	if fMode {
		patterns, err = loadPatternsFile(*patternsFile)
		if err != nil {
			fatal("%v", err)
		}
		// -o in -f mode must be a directory (one file per term).
		if *outFile != "" {
			patternFDCount = len(patterns)
			var perr error
			w, perr = clampWorkersForFD(w, patternFDCount)
			if perr != nil {
				usage("%v", perr)
			}
			abs, derr := validateFileOutputDir(*outFile)
			if derr != nil {
				usage("%v", derr)
			}
			// Per-pattern files are created before discovery, so an -o dir
			// under the search root (by identity, not just spelling) would
			// itself be discovered and (in -txt mode) scanned mid-write.
			// Reject the nesting up front; an unresolvable path fails closed.
			// This must run before ensureFileOutputDir materializes anything,
			// so a rejected invocation leaves no directory behind.
			under, uerr := outputDirUnderRoot(abs, args.Root)
			if uerr != nil {
				usage("%v", uerr)
			}
			if under {
				usage("-f -o must be outside the search root (got %s under %s)", abs, args.Root)
			}
			// H-04: the per-pattern outputs below are opened with truncation
			// by allocatePatternFiles, and the -f patterns file was already
			// read — a -json target naming either would destroy input or
			// corrupt a fresh output the moment the stream opens. Refuse
			// here, before anything is created, against the full prospective
			// candidate set. The config file rides along: it is an input too,
			// and the check is free at this point.
			protected := append([]string{*patternsFile, cfg.Path()},
				prospectivePatternFilePaths(abs, patterns, started)...)
			if jerr := rejectJSONOutPreflightCollisions(jsonOutTarget(jsonOutFlag), protected...); jerr != nil {
				usage("%v", jerr)
			}
			if eerr := ensureFileOutputDir(abs); eerr != nil {
				usage("%v", eerr)
			}
			files, paths, ferr := allocatePatternFiles(abs, patterns, started)
			if ferr != nil {
				fatal("%v", ferr)
			}
			multiSink = newDispatchSink(files, *clean)
			multiPaths = paths
			// Signal run() to use the dispatchSink instead of a single -o file.
			*outFile = ""
		}
		// Build the multi-pattern matcher (one pass over archives).
		patBytes := make([][]byte, len(patterns))
		for i, p := range patterns {
			patBytes[i] = []byte(p)
		}
		matcher = search.NewMultiMatcher(patBytes)
	}
	// -json owns stdout, so the hits need an explicit destination: -o
	// FILE or [sfs] o in config (jsonOutDestGiven, captured before the -f
	// per-term routing cleared *outFile). Refuse before anything is opened
	// or truncated; the -f rule above already demanded -o DIR for its
	// per-term files with its own message.
	if jsonOutFlag.Enabled && !jsonOutDestGiven {
		usage("-json owns stdout; give the hits a destination: -o FILE (or [sfs] o in config)")
	}

	pattern := args.Pattern
	if !fMode {
		if pattern == "" {
			fatal("empty pattern")
		}
		patterns = []string{pattern}
	}
	matchAll := !fMode && pattern == "*"
	if matchAll && *limit == 0 && *since == "" {
		fmt.Fprintln(os.Stderr, "note: '*' exports all lines; use -l N or -since DUR to narrow scope")
	}

	var modifiedAfter time.Time
	if *since != "" {
		dur, perr := parseSince(*since)
		if perr != nil {
			usage("%v", perr)
		}
		modifiedAfter = time.Now().Add(-dur)
	}

	var archives []string
	switch {
	case *txtMode && !modifiedAfter.IsZero():
		archives, err = discover.ListTxtSince(args.Root, modifiedAfter)
	case *txtMode:
		archives, err = discover.ListTxt(args.Root)
	case !modifiedAfter.IsZero():
		archives, err = discover.ListZstSince(args.Root, modifiedAfter)
	default:
		archives, err = discover.ListZst(args.Root)
	}
	if err != nil {
		if !discover.IsEmptyResult(err) {
			fatal("%v", err)
		}
		if !modifiedAfter.IsZero() {
			// #11: an empty -since window is an empty result, not an error —
			// exit 0 with one honest stderr note (no wrong-path guess; the
			// window is simply tight).
			fmt.Fprintf(os.Stderr, "note: %v\n", err)
			reg.ExitWithCode(0)
		}
		// #12: an empty root discovered nothing — the shared contract
		// (internal/exitcode) and sfu/sfl say 4 = nothing usable. The
		// actionable message stays.
		if joutRef != nil {
			joutRef.stopWithCode(tuistat.EventError, fmt.Sprintf("%v", err), exitcode.NothingUsable)
		}
		fmt.Fprintf(os.Stderr, "sfs: %v\n", err)
		reg.ExitWithCode(exitcode.NothingUsable)
	}

	// fd preflight, worst case ~2*W + sidecars + stdio plus per-pattern outputs.
	var fdErr error
	w, fdErr = clampWorkersForFD(w, patternFDCount)
	if fdErr != nil {
		usage("%v", fdErr)
	}

	// -txt mode warns if .zst archives lurk in same root, inverse stays silent
	if *txtMode {
		if zstFiles, zerr := discover.ListZst(args.Root); zerr == nil && len(zstFiles) > 0 {
			fmt.Fprintf(os.Stderr, "note: -txt mode; %d .zst archive(s) under %s ignored\n", len(zstFiles), args.Root)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fatal("getwd: %v", err)
	}
	// -stats selects the old file+TUI path; legacy -s/-silent are no-ops (stream is default).
	statsMode := *stats
	// Hit routing with -json: stdout belongs to the stream, so hits go
	// file-only to the explicit -o (never generated, never tee'd), the same
	// shape as -stats -o. In -f -o DIR mode the per-term files already own
	// the hits, so the stream takes stdout without touching -o.
	jsonOutHits := jsonOutFlag.Enabled && multiSink == nil
	outputMode, err := resolveOutputMode(*outFile, statsMode, jsonOutHits, cwd, started)
	if err != nil {
		fatal("%v", err)
	}
	*outFile = outputMode.OutFile
	streamMode := outputMode.Stream
	statsMode = outputMode.Stats
	// Single-pattern/-stats -o always names a FILE (teed with the default
	// stream, file-only with -stats). Reject directory-shaped targets before
	// anything is created so a bad spelling cannot leave debris mid-run.
	if multiSink == nil && *outFile != "" {
		if err := validateSingleOutputFile(*outFile); err != nil {
			usage("%v", err)
		}
	}
	// dont clobber search target w/ -o/default output, O_TRUNC fires pre-scan
	if err := ensureNoOutputCollision(*outFile, archives); err != nil {
		fatal("%v", err)
	}
	// -json policy (exit 2, checked BEFORE the stream truncates its
	// target): a non-positive interval is a usage error, and a stream file
	// equal to the hit output or living under the search root would destroy
	// data — the same pre-flight rejection sfu applies to its inputs.
	if jsonOutFlag.Enabled && *jsonEvery <= 0 {
		usage("invalid -json-every %v: must be positive", *jsonEvery)
	}
	if err := rejectJSONOutTargetCollisions(jsonOutTarget(jsonOutFlag), *outFile, args.Root); err != nil {
		usage("%v", err)
	}
	// The -debug log will be created in CWD (exclusive creation, so it can
	// never truncate an existing file) right after the stream opens; a
	// -json target spelling its predictable name would race it for the
	// path. Refuse that spelling before the stream opens. The loaded config
	// file is an input as well and is checked in both modes here (the -f
	// block already covered it alongside the per-pattern candidates).
	if target := jsonOutTarget(jsonOutFlag); target != "" && target != "-" {
		cwd, gerr := os.Getwd()
		if gerr != nil {
			fatal("getwd: %v", gerr)
		}
		debugBase := filepath.Join(cwd, "sfs-debug-"+debugStamp(started)+".log")
		if jerr := rejectJSONOutPreflightCollisions(target, debugBase, cfg.Path()); jerr != nil {
			usage("%v", jerr)
		}
	}
	jout, err := newJSONOut(jsonOutTarget(jsonOutFlag), *jsonEvery, reg)
	if err != nil {
		fatal("%v", err)
	}

	updateChecker := selfupdate.NewChecker(version.String, os.Args[0], *noUpdateCheck)
	updateChecker.Start()
	ctx, cancel, signaled := reg.SignalContext()
	defer cancel()

	files := &fileabort.Registry{}
	ctx = fileabort.WithContext(ctx, files)
	go reg.WatchInterrupt(ctx, files, signaled)

	// -stats requests the live screen. -json hides it only when the stream
	// target is stdout and stdout is a terminal. The plain end-of-run summary
	// still prints to stderr.
	target := ""
	if jsonOutFlag.Enabled {
		target = jsonOutTarget(jsonOutFlag)
	}
	uiMode := resolveRunUIMode(statsMode, target, stdoutIsCharDevice(), vtOK)

	var dbg *debugLog
	var debugLogPath string
	if *debugFlag {
		if cwd == "" {
			cwd, err = os.Getwd()
			if err != nil {
				fatal("getwd: %v", err)
			}
		}
		f, p, err := ulpengine.CreateArtifactFile(cwd, "sfs-debug-"+debugStamp(started), ".log", 0o600)
		if err != nil {
			fatal("debug log: %v", err)
		}
		debugLogPath = p
		dbg = newDebugLogFile(f)
		defer func() { _ = dbg.Close() }()
		// ExitWithCode (fatal/usage) and ForceExit (second Ctrl-C, cleanup
		// timeout) end in os.Exit, which skips the deferred dbg.Close. Arm the
		// registry's pre-exit flush: today every sfs debug write path flushes
		// (Event, header, progress, termination, completion), so no reachable
		// seam loses data — this keeps it true if a buffered write path is
		// ever added. Silent and best-effort by contract.
		reg.SetExitFlush(func() { dbg.Flush() })
	}

	debugInfo := debugRunInfo{
		root:       args.Root,
		pattern:    pattern,
		patternLen: len(pattern),
		workers:    w,
		outFile:    *outFile,
		stream:     streamMode,
		stats:      statsMode,
		clean:      *clean,
		combo:      *combo,
		gomaxprocs: runtime.GOMAXPROCS(0),
		uiMode:     uiModeString(uiMode),
		stderrTTY:  stderrIsTTY(),
		txtMode:    *txtMode,
		archives:   archives,
	}
	for _, arch := range archives {
		if *txtMode {
			if st, err := os.Stat(arch); err == nil {
				debugInfo.indexBytesTotal += st.Size()
			}
		} else if sz, err := index.ArchiveSize(arch); err == nil {
			debugInfo.indexBytesTotal += sz
		}
	}
	if dbg != nil {
		dbg.writeHeader(filepath.Base(os.Args[0]), started, os.Args, debugInfo)
		fmt.Fprintf(os.Stderr, "debug log: %s\n", debugLogPath)
	}

	metrics := &search.Metrics{}

	// Open the stream before the run so the index phase is streamed; joutRef
	// arms main's panic hook once start returned.
	jout.setMetrics(metrics)
	jout.start()
	joutRef = jout

	runErr := run(ctx, runConfig{
		root:            args.Root,
		pattern:         pattern,
		matchAll:        matchAll,
		archives:        archives,
		txtMode:         *txtMode,
		workers:         w,
		outFile:         *outFile,
		stream:          streamMode,
		clean:           *clean,
		combo:           *combo,
		decodeStep:      *decodeStep,
		maxHitsPerChunk: *maxHitsPerChunk,
		limit:           *limit,
		signaled:        signaled,
		started:         started,
		debug:           dbg,
		metrics:         metrics,
		stdout:          os.Stdout,
		indexBytesTotal: debugInfo.indexBytesTotal,
		uiMode:          uiMode,
		matcher:         matcher,
		multiSink:       multiSink,
	})
	wall := time.Since(started)

	if runErr != nil {
		if dbg != nil {
			dbg.logTermination(runErr, signaled(), wall, metrics)
		}
		if signaled() {
			fmt.Fprintln(os.Stderr, "\ninterrupted")
			jout.stop(tuistat.EventInterrupted, "")
			reg.ExitWithCode(termctl.InterruptExitCode())
		}
		// Partial scan: show the final summary with an INCOMPLETE terminal
		// line (hits found so far are valid output), then exit 1. Exit 0 /
		// COMPLETE is reserved for scans where every selected file/chunk was
		// scanned or deliberately capped by the documented -l hit cap.
		var perr *search.PartialScanError
		if errors.As(runErr, &perr) {
			if statsMode {
				summaryOut, _ := finalizeEmptyOutput(*outFile, outputMode.Generated, metrics.Hits.Load())
				for _, ln := range renderIncompleteSummary(started, metrics, summaryOut, pattern, updateChecker.NoticeForSummary()) {
					fmt.Fprintln(os.Stderr, ln)
				}
			}
			fatal("%v", perr)
		}
		fatal("%v", runErr)
	}
	if dbg != nil {
		dbg.logCompletion(metrics, wall, debugInfo)
	}
	// The run completed: the terminal + summary rollup close the stream
	// before the human recap prints (mirror sfu/sfl). M-10: a capped run's
	// results are incomplete, so the terminal carries the partial exit code
	// instead of "done" — machine consumers must not classify truncated
	// output as successful.
	if metrics.ChunksCapped.Load() > 0 {
		jout.stopWithCode(tuistat.EventError, "search truncated: -max-hits-per-chunk cap reached", exitcode.Partial)
	} else {
		jout.stop(tuistat.EventDone, "")
	}
	if multiSink != nil {
		// -f -o DIR: one summary line per pattern file, mirroring the
		// per-term summary style ("N hits → path"). Hits are already flushed
		// and files closed by run(); we just report.
		printMultiPatternSummary(metrics.Hits.Load(), multiPaths)
	}
	if statsMode {
		// A generated-default output with zero hits would leave a 0-byte
		// sfs_results_*.txt cluttering CWD; remove it (run() has returned, so
		// its deferred Close already ran — safe to unlink on Windows too) and
		// surface "(no matches)" in place of the output path. An explicit -o is
		// left untouched: the user asked for that file.
		summaryOut, removed := finalizeEmptyOutput(*outFile, outputMode.Generated, metrics.Hits.Load())
		if removed && dbg != nil {
			dbg.Event("no hits: removed empty generated output %q", *outFile)
		}
		// NoticeForSummary returns nil when the check is disabled, so no extra guard.
		updateNotice := updateChecker.NoticeForSummary()
		for _, ln := range renderFinalSummary(started, metrics, summaryOut, pattern, updateNotice) {
			fmt.Fprintln(os.Stderr, ln)
		}
	}
	if bellEnabled {
		bell.Ring()
	}
	// Exit-code policy (internal/exitcode): -max-hits-per-chunk truncation
	// means the results are incomplete. The run still succeeded (hits above
	// are valid output), so this is the partial-failure code, not an error.
	if metrics.ChunksCapped.Load() > 0 {
		reg.ExitWithCode(exitcode.Partial)
	}
}

type runConfig struct {
	root            string
	pattern         string
	matchAll        bool
	archives        []string
	txtMode         bool
	workers         int
	outFile         string
	stream          bool
	clean           bool
	combo           bool
	decodeStep      int
	maxHitsPerChunk int
	limit           int
	signaled        func() bool
	started         time.Time
	debug           *debugLog
	metrics         *search.Metrics
	// stdout is the hit stream sink for stream/tee mode. Defaults to os.Stdout
	// in main; tests inject a buffer so they never mutate the process-global
	// os.Stdout (which would race with t.Parallel).
	stdout io.Writer
	// indexBytesTotal and uiMode are resolved by the caller (main) since it
	// already computes them for the debug header; run() consumes them instead
	// of re-statting every archive and re-resolving the UI mode.
	indexBytesTotal int64
	uiMode          uiMode
	// matcher enables multi-pattern (-f) mode when non-nil. Each line is
	// matched against every pattern in one pass; hits carry PatternIdx for
	// per-file routing. nil = single-pattern (cfg.pattern via BMH).
	matcher *search.MultiMatcher
	// multiSink is the per-pattern file sink used only in -f + -o DIR mode.
	// When non-nil, cfg.outFile is "" and the single-file path is skipped.
	// nil for single-pattern and -f-without-o (stream to stdout).
	multiSink *dispatchSink
	// comboSkippedOut, when non-nil, receives the count of hit lines skipped
	// by -combo (lines that are not U:L:P). main leaves it nil and reads the
	// end-of-run note path instead; tests inject an atomic to observe it
	// without racing the process-global stderr.
	comboSkippedOut *atomic.Int64
}

func clampWorkersForFD(workers, patternCount int) (int, error) {
	maxFD, ok := fdlimit.MaxOpenFiles()
	if !ok || maxFD <= 0 {
		return workers, nil
	}
	return clampWorkersForFDAtLimit(workers, patternCount, maxFD)
}

// clampWorkersForFDAtLimit is the maxFD-parameterized core of
// clampWorkersForFD, split out so tests can drive pathological limits
// without touching the process rlimit.
func clampWorkersForFDAtLimit(workers, patternCount, maxFD int) (int, error) {
	const fixedReserve = 16 // stdio + sidecars + decoder slots
	allowance := maxFD - fixedReserve - patternCount
	if patternCount > 0 && allowance < 1 {
		return workers, fmt.Errorf("-f: requested %d pattern output files exceed RLIMIT_NOFILE=%d (fixed descriptor reserve %d)", patternCount, maxFD, fixedReserve)
	}
	if patternCount == 0 && allowance < 2 {
		// A worker needs 2 descriptors (pair); reject up front instead of
		// floor-clamping to 1 and failing later with EMFILE.
		return workers, fmt.Errorf("RLIMIT_NOFILE=%d leaves insufficient file-descriptor allowance for workers (allowance %d after fixed reserve %d)", maxFD, allowance, fixedReserve)
	}
	safeWorkers := allowance / 2
	if safeWorkers < 1 {
		safeWorkers = 1
	}
	if safeWorkers < workers {
		fmt.Fprintf(os.Stderr, "note: clamping -j from %d to %d to fit RLIMIT_NOFILE=%d\n", workers, safeWorkers, maxFD)
		workers = safeWorkers
	}
	return workers, nil
}

func run(ctx context.Context, cfg runConfig) error {
	// child ctx so we can stop the search early on -l without disturbing the
	// signal-driven parent ctx (interrupt handling stays in main).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	archiveOrd := make(map[string]int, len(cfg.archives))
	for i, a := range cfg.archives {
		archiveOrd[a] = i
	}

	// Hit sink selection. Three shapes:
	//   - single-pattern: one search.Writer over stdout / -o file / tee.
	//   - multi-pattern (-f) without -o: one search.Writer over stdout (all
	//     hits interleaved).
	//   - multi-pattern (-f) + -o DIR: a dispatchSink over one file per pattern
	//     (cfg.multiSink, set by main; no stdout, no single cfg.outFile).
	// stdoutIsTheSink drives per-hit flush (pipe-fix): only when the sink
	// wraps stdout do we flush every hit so piped output isn't block-buffered.
	stdout := cfg.stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	var sink hitSink
	var stdoutIsTheSink bool
	// out is the single-pattern -o file (nil in -f -o DIR mode, where the
	// dispatchSink owns its own files). Kept at function scope so the
	// interrupted-output defer can discard it.
	var out *os.File

	if cfg.multiSink != nil {
		// -f -o DIR: per-pattern files, no stdout. cfg.outFile is "" by
		// construction; the single-file open block below is skipped.
		sink = cfg.multiSink
		stdoutIsTheSink = false
		// Always close the per-pattern files on return. The interrupted-output
		// defer (registered below) handles Close+Remove on a signal; this
		// defer runs last (registered first) and is a no-op then (Close is
		// idempotent). On a clean return it closes so a flush error or a
		// searchErr return path can't strand the files open.
		defer cfg.multiSink.Close()
	} else {
		if cfg.outFile != "" {
			dir := filepath.Dir(cfg.outFile)
			if dir != "." {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("create output dir: %w", err)
				}
			}
			f, err := os.Create(cfg.outFile)
			if err != nil {
				return fmt.Errorf("open output: %w", err)
			}
			defer f.Close()
			out = f
		}
		var resultWriter io.Writer = stdout
		switch {
		case cfg.stream && out != nil:
			resultWriter = io.MultiWriter(stdout, out)
		case out != nil:
			resultWriter = out
		}
		sink = search.NewWriter(resultWriter, cfg.clean)
		// Per-hit flush only when stdout is part of the sink (stream or tee);
		// file-only (stats) buffers for throughput.
		stdoutIsTheSink = cfg.stream
	}

	uiMode := cfg.uiMode

	metrics := cfg.metrics
	if metrics == nil {
		metrics = &search.Metrics{}
	}
	metrics.ArchivesTotal.Store(int64(len(cfg.archives)))

	indexBytesTotal := cfg.indexBytesTotal
	metrics.IndexBytesTotal.Store(indexBytesTotal)

	if cfg.txtMode {
		n := int64(len(cfg.archives))
		metrics.Phase.Store(search.PhaseSearch)
		metrics.ArchivesIndexed.Store(n)
		metrics.IndexBytesDone.Store(indexBytesTotal)
		metrics.ChunksTotal.Store(n)
		metrics.BytesScannedTotal.Store(indexBytesTotal)
	} else {
		metrics.Phase.Store(search.PhaseIndex)
	}

	stopDebug := startDebugProgress(ctx, cfg.debug, metrics)
	defer stopDebug()
	if cfg.debug != nil {
		if cfg.txtMode {
			cfg.debug.Event("discovered %d .txt files under %q", len(cfg.archives), cfg.root)
		} else {
			cfg.debug.Event("discovered %d archives under %q", len(cfg.archives), cfg.root)
		}
	}

	// Notices (chunk-cap, skipped corrupt archives) are collected during the run
	// and printed only after the alt-screen is torn down, so they never corrupt a
	// live TUI frame. addNote is safe to call from worker goroutines.
	var noteMu sync.Mutex
	var notes []string
	addNote := func(s string) {
		noteMu.Lock()
		notes = append(notes, s)
		noteMu.Unlock()
	}

	uiDone := make(chan struct{})
	var uiWG sync.WaitGroup
	uiWG.Add(1)
	go runUI(uiConfig{
		Mode:     uiMode,
		Metrics:  metrics,
		Pattern:  cfg.pattern,
		Start:    cfg.started,
		Done:     uiDone,
		Signaled: cfg.signaled,
	}, &uiWG)
	defer func() {
		close(uiDone)
		uiWG.Wait()
		noteMu.Lock()
		for _, n := range notes {
			fmt.Fprintln(os.Stderr, n)
		}
		noteMu.Unlock()
	}()

	var sidecars map[string]*index.Sidecar
	// indexPartial holds per-archive index-build failures; it is merged with
	// search-phase failures and returned after healthy hits are flushed.
	var indexPartial *search.PartialScanError
	if !cfg.txtMode {
		sidecars, indexPartial = indexArchives(ctx, cfg.archives, cfg.workers, metrics, cfg.debug)
		if len(sidecars) == 0 {
			// nothing was indexed: the partial failures ARE the reason
			if indexPartial != nil {
				return indexPartial
			}
			return errors.New("no indexes available")
		}
		if cfg.debug != nil {
			var chunks int64
			for _, sc := range sidecars {
				chunks += int64(len(sc.Chunks))
			}
			cfg.debug.Event("index complete: %d sidecars, %d chunks", len(sidecars), chunks)
			cfg.debug.logProgress(metrics)
		}
	}

	hitCh := make(chan search.Hit, 4096)
	// Ordered archive grouping only in stats (file-only) mode; stream/tee stay
	// unordered for live throughput like the old -s path. -f mode is always
	// stream (stats rejected), so it stays unordered too.
	orderedOutput := !cfg.stream
	// Per-hit flush when the sink wraps stdout, so piped output isn't
	// block-buffered (1 MiB) and sparse-hit queries over huge archives surface
	// live instead of appearing stuck. File-only and per-pattern-file modes
	// keep end-of-run flush for throughput.
	streamFlush := cfg.stream && stdoutIsTheSink

	var printer *search.OrderedPrinter
	// writeHit routes one hit into the active sink. With -combo the hit line
	// is replaced by its login:password half. Raw -txt hits use strict parsing;
	// archive/library hits use the trusted stored-record decoder. Lines that
	// don't parse as credential records are skipped and counted in the notes.
	var comboSkipped atomic.Int64
	comboDecoder := comboOf
	if !cfg.txtMode {
		comboDecoder = comboOfStored
	}
	writeHit := func(h search.Hit) error {
		if cfg.combo {
			lp, ok := comboDecoder(h.Line)
			if !ok {
				n := comboSkipped.Add(1)
				if cfg.comboSkippedOut != nil {
					cfg.comboSkippedOut.Store(n)
				}
				return nil
			}
			h.Line = lp
		}
		return sink.WriteHit(h)
	}
	// archiveDoneCh carries "this archive is fully processed" from the search
	// workers to the drain loop. Only used for ordered -o output, where it lets
	// the OrderedPrinter flush and release completed archives mid-run instead of
	// holding every hit in memory until the search finishes. Buffered past the
	// archive count so a worker's done-callback never blocks.
	var archiveDoneCh chan int
	if orderedOutput {
		archiveDoneCh = make(chan int, len(cfg.archives)+1)
		printer = search.NewOrderedPrinter(writeHit)
		writeHit = printer.Add
	}
	onArchiveDone := func(ord int) {
		if archiveDoneCh != nil {
			archiveDoneCh <- ord
		}
	}

	var firstHit sync.Once
	if cfg.debug != nil {
		if cfg.txtMode {
			cfg.debug.Event("search start workers=%d files=%d patternLen=%d (txt mode)", cfg.workers, len(cfg.archives), len(cfg.pattern))
		} else {
			var totalChunks int64
			for _, sc := range sidecars {
				totalChunks += int64(len(sc.Chunks))
			}
			cfg.debug.Event("search start workers=%d chunks=%d patternLen=%d", cfg.workers, totalChunks, len(cfg.pattern))
		}
	}

	var searchErr error
	var searchWG sync.WaitGroup
	searchWG.Add(1)
	go func() {
		defer searchWG.Done()
		// file output uses OrderedPrinter, MarkArchiveDone only after hit drain
		// (early advance strands late hits)
		if cfg.txtMode {
			searchErr = search.RunTxt(search.TxtConfig{
				Ctx:          ctx,
				MatchAll:     cfg.matchAll,
				Pattern:      []byte(cfg.pattern),
				MultiMatcher: cfg.matcher,
				Workers:      cfg.workers,
				Files:        cfg.archives,
				Metrics:      metrics,
				Hits:         hitCh,
				ArchiveOrd:   archiveOrd,
				// H-09: the per-file hit cap in txt mode mirrors the
				// per-chunk cap in compressed mode, with the same
				// partial-exit telemetry (ChunksCapped → exit 3).
				MaxHitsPerFile: cfg.maxHitsPerChunk,
				OnFileDone:     onArchiveDone,
				OnFileError: func(path string, err error) {
					if cfg.debug != nil {
						cfg.debug.Event("file error path=%s err=%v", filepath.Base(path), err)
					}
				},
				OnFileCapped: func(path string, emitted int) {
					metrics.ChunksCapped.Add(1)
					addNote(fmt.Sprintf("note: %s: hit cap reached (%d hits); file truncated",
						filepath.Base(path), emitted))
					if cfg.debug != nil {
						cfg.debug.Event("file capped path=%s emitted=%d", filepath.Base(path), emitted)
					}
				},
			})
		} else {
			searchErr = search.Run(search.Config{
				Ctx:             ctx,
				DecodeStep:      cfg.decodeStep,
				MaxHitsPerChunk: cfg.maxHitsPerChunk,
				MatchAll:        cfg.matchAll,
				Pattern:         []byte(cfg.pattern),
				MultiMatcher:    cfg.matcher,
				Workers:         cfg.workers,
				Archives:        cfg.archives,
				Sidecars:        sidecars,
				Metrics:         metrics,
				Hits:            hitCh,
				ArchiveOrd:      archiveOrd,
				OnArchiveDone:   onArchiveDone,
				OnChunkError: func(archive string, chunkID int, err error) {
					if cfg.debug != nil {
						cfg.debug.Event("chunk error archive=%s chunk=%d err=%v", filepath.Base(archive), chunkID, err)
					}
				},
				OnChunkCapped: func(archive string, chunkID int, emitted int) {
					metrics.ChunksCapped.Add(1)
					addNote(fmt.Sprintf("note: %s chunk %d: hit cap reached (%d hits); chunk truncated",
						filepath.Base(archive), chunkID, emitted))
					if cfg.debug != nil {
						cfg.debug.Event("chunk capped archive=%s chunk=%d emitted=%d", filepath.Base(archive), chunkID, emitted)
					}
				},
			})
		}
		close(hitCh)
	}()

	var emitted int
	limitReached := false
	defer func() {
		if ctx.Err() != nil && !limitReached {
			if cfg.multiSink != nil {
				// -f -o DIR: discard every per-pattern file so an interrupted
				// run leaves no half-written splits behind. Close is
				// idempotent and nils the file slots, so the always-close
				// defer registered at sink selection is a clean no-op after.
				_ = cfg.multiSink.Flush()
				for i, f := range cfg.multiSink.files {
					if f != nil {
						_ = f.Close()
						_ = os.Remove(f.Name())
						cfg.multiSink.files[i] = nil
					}
				}
				return
			}
			if cfg.outFile != "" {
				discardInterruptedOutput(cfg.outFile, out)
			}
		}
	}()

	// handleHit records one hit. Returns stop=true when the -l limit is reached
	// (cancel() halts workers; limitReached makes the ctx-cancelled state below a
	// clean exit). Shared by the main drain and the archive-done pre-drain.
	handleHit := func(h search.Hit) (stop bool, err error) {
		firstHit.Do(func() {
			if cfg.debug != nil {
				cfg.debug.Event("first hit archive=%s chunk=%d offset=%d", filepath.Base(h.Archive), h.ChunkID, h.Offset)
			}
		})
		if err := writeHit(h); err != nil {
			return false, fmt.Errorf("write hit: %w", err)
		}
		emitted++
		if streamFlush {
			if err := sink.Flush(); err != nil {
				return false, fmt.Errorf("flush hit: %w", err)
			}
		}
		if cfg.limit > 0 && emitted >= cfg.limit {
			limitReached = true
			cancel()
			return true, nil
		}
		return false, nil
	}

	// drainBuffered consumes hits already queued on hitCh, non-blocking. A done
	// signal for an archive is sent only after every one of its chunks/files has
	// finished, so all that archive's hits are guaranteed already on hitCh by
	// then — draining them here before MarkArchiveDone is what keeps a late hit
	// from being stranded past an already-flushed archive.
	drainBuffered := func() (stop bool, err error) {
		for {
			select {
			case h, ok := <-hitCh:
				if !ok {
					return false, nil
				}
				if stop, err := handleHit(h); err != nil || stop {
					return stop, err
				}
			default:
				return false, nil
			}
		}
	}

drainHits:
	for {
		select {
		case <-ctx.Done():
			break drainHits
		case ord := <-archiveDoneCh:
			// Flush and release this archive (plus any contiguous completed
			// ones) mid-run, so -o output doesn't accumulate every hit in RAM.
			if stop, err := drainBuffered(); err != nil {
				return err
			} else if stop {
				break drainHits
			}
			if err := printer.MarkArchiveDone(ord); err != nil {
				return fmt.Errorf("write hit: %w", err)
			}
		case h, ok := <-hitCh:
			if !ok {
				break drainHits
			}
			if stop, err := handleHit(h); err != nil {
				return err
			} else if stop {
				break drainHits
			}
		}
	}

	searchWG.Wait()
	if limitReached {
		// workers may have counted buffered-but-undrained hits before stopping;
		// pin the reported total to what we actually emitted.
		metrics.Hits.Store(int64(emitted))
	}
	if cfg.debug != nil {
		cfg.debug.Event("search complete hits=%d chunks=%d/%d scanned=%d",
			metrics.Hits.Load(), metrics.ChunksDone.Load(), metrics.ChunksTotal.Load(), metrics.BytesScanned.Load())
	}
	if cfg.combo {
		if n := comboSkipped.Load(); n > 0 {
			addNote(fmt.Sprintf("note: -combo skipped %d hit line(s) that are not U:L:P", n))
		}
	}
	if ctx.Err() != nil && !limitReached {
		return ctx.Err()
	}

	if orderedOutput {
		for ord := 0; ord < len(cfg.archives); ord++ {
			if err := printer.MarkArchiveDone(ord); err != nil {
				return fmt.Errorf("write hit: %w", err)
			}
		}
	}
	if err := sink.Flush(); err != nil {
		return fmt.Errorf("flush output: %w", err)
	}
	// -f -o DIR: per-pattern files are closed by the deferred Close registered
	// at sink selection (idempotent w/ the interrupt defer's Close+Remove).
	// All hits are flushed at this point: only now do partial scan failures
	// fail the run. Already-emitted hits remain valid output. The -l hit cap
	// is deliberate truncation (documented), so limitReached stays a clean
	// exit — same for index failures, which the cap preempts.
	if !limitReached {
		var searchPartial *search.PartialScanError
		if searchErr != nil && errors.As(searchErr, &searchPartial) {
			searchPartial = mergePartial(indexPartial, searchPartial)
		}
		if searchPartial != nil {
			return searchPartial
		}
		if indexPartial != nil {
			return indexPartial
		}
	}
	if searchErr != nil && !limitReached {
		return searchErr
	}
	return nil
}

// mergePartial combines two partial-scan failures (index phase + search
// phase) into one; nil inputs are skipped.
func mergePartial(a, b *search.PartialScanError) *search.PartialScanError {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return &search.PartialScanError{Failures: append(a.Failures, b.Failures...)}
}

// discardInterruptedOutput removes a partial -o file after a graceful Ctrl-C so
// interrupted runs do not leave half-written hit lists behind.
func discardInterruptedOutput(path string, f *os.File) {
	if path == "" {
		return
	}
	if f != nil {
		_ = f.Close()
	}
	ulpengine.RemovePathLogged(path)
}

func indexArchives(ctx context.Context, archives []string, workers int, metrics *search.Metrics, dbg *debugLog) (map[string]*index.Sidecar, *search.PartialScanError) {
	sidecars := make(map[string]*index.Sidecar, len(archives))
	var sidecarMu sync.Mutex
	// index failures are collected thread-safely and returned to run(), which
	// fails the whole run after healthy hits are flushed — a failed index build
	// means that archive was never searched, so the scan is incomplete.
	var failures search.FailureCollector
	indexJobs := make(chan string, len(archives))
	var indexWG sync.WaitGroup

	for i := 0; i < workers; i++ {
		indexWG.Add(1)
		go func() {
			defer indexWG.Done()
			for arch := range indexJobs {
				if ctx.Err() != nil {
					return
				}
				metrics.IndexArchivesActive.Add(1)
				archSize, _ := index.ArchiveSize(arch)
				progress := index.NewArchiveByteProgress(&metrics.IndexBytesDone)
				act := indexActivity(metrics)
				sc, meta, err := index.Ensure(ctx, arch, progress.Callback(), act)
				if err != nil {
					metrics.IndexArchivesActive.Add(-1)
					if ctx.Err() != nil {
						return
					}
					if dbg != nil {
						dbg.Event("index failed archive=%s err=%v", filepath.Base(arch), err)
					}
					failures.Add(arch, -1, err)
					continue
				}
				progress.Finish(archSize)
				metrics.IndexArchivesActive.Add(-1)
				if ctx.Err() != nil {
					return
				}
				sidecarMu.Lock()
				sidecars[arch] = sc
				sidecarMu.Unlock()
				metrics.ArchivesIndexed.Add(1)
				if dbg != nil {
					dbg.logIndexEvent(arch, meta, len(sc.Chunks))
				}
			}
		}()
	}
	for _, arch := range archives {
		indexJobs <- arch
	}
	close(indexJobs)
	indexWG.Wait()
	if ctx.Err() != nil {
		if dbg != nil {
			dbg.Event("index interrupted: %v", ctx.Err())
		}
		// cancellation wins: an interrupted run reports interrupted, not partial
		return sidecars, nil
	}
	return sidecars, failures.Result()
}

func fatal(format string, args ...any) {
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
	fmt.Fprintf(os.Stderr, "sfs: "+format+"\n", args...)
	reg.ExitWithCode(1)
}

// argv-shape error, exits 2 vs runtime errors (1) so scripts can branch
func usage(format string, args ...any) {
	// Same funnel as fatal for the usage exit code; every usage path runs
	// before the stream opens, so this is a no-op in practice.
	if joutRef != nil {
		joutRef.stopWithCode(tuistat.EventError, fmt.Sprintf(format, args...), exitcode.Usage)
	}
	fmt.Fprintf(os.Stderr, "sfs: "+format+"\n", args...)
	fmt.Fprintln(os.Stderr, "run with -h for help")
	reg.ExitWithCode(2)
}

// resolveFModeStats applies the -f mode policy to the effective stats value.
// An explicitly supplied -stats is a usage error; stats inherited from config
// is disabled silently because -f has its own stream/per-term output modes.
func resolveFModeStats(stats, statsVisited bool) (bool, error) {
	if stats && statsVisited {
		return false, errors.New("explicit -stats is incompatible with -f")
	}
	return false, nil
}
