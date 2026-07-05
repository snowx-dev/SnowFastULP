package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/console"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// reg is the shared terminal restore/exit registry: the live frame registers
// its teardown via Set, and every exit path (graceful ExitWithCode, force-exit
// on a second Ctrl-C, cleanup timeout, fatal/usage) routes through it so the
// alt-screen is always left cleanly. ulpengine.PrintManualCleanupHint prints
// stranded scratch paths on a force-exit.
var reg = termctl.New(os.Stderr, ulpengine.PrintManualCleanupHint)

// validates output dir flag, empty = CWD. must look like a dir, plain
// file paths rejected. flagName used in error msg
func resolveOutputDir(flagName, userOut string) (dir string, autoMkdir bool, err error) {
	userOut = strings.TrimSpace(userOut)
	if userOut == "" {
		return ".", true, nil
	}
	if !isDirHint(userOut) {
		return "", false, fmt.Errorf(
			"%s must be a directory (trailing %q or existing directory); got %q — use e.g. %s ./out/",
			flagName, string(os.PathSeparator), userOut, flagName)
	}
	return userOut, true, nil
}

// looks like a dir: trailing separator, or stats as existing dir.
// stat errors fall through to false
func isDirHint(p string) bool {
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, string(os.PathSeparator)) {
		return true
	}
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return true
	}
	return false
}

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

func main() {
	// A panic anywhere below (e.g. inside ulpengine.Run) would otherwise crash
	// with the alt-screen still up and the cursor hidden. Restore first, then
	// re-panic so the stack trace prints on a clean screen. No-op until the
	// monitor installs the hook.
	defer func() {
		if r := recover(); r != nil {
			reg.Restore()
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

	flag.Usage = func() { printHelp(filepath.Base(os.Args[0]), os.Stderr) }

	// resolve --version / --help before loading cfg so bad cfg
	// doesnt block diagnostic output
	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastULP %s\n", version.String)
		return
	}
	if cliargs.IsHelpRequest(os.Args[1:]) {
		printHelp(filepath.Base(os.Args[0]), os.Stdout)
		reg.ExitWithCode(0)
	}

	// `update` / `upgrade`: replace installed SnowFast binaries with the latest release.
	// Handled before cfg load so a bad config can't block self-update.
	if handled, err := selfupdate.Dispatch(os.Args[1:], version.String, os.Stdout); handled {
		if err != nil {
			fatalf("%v", err)
		}
		return
	}

	// Gate color on stderr (the TUI + summary target): a redirected stderr log
	// must never accumulate ANSI escapes even when stdout is a TTY.
	applyStderrColorProfile()

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
	loose := flag.Bool("loose", false, "high-recall parser: accepts host:port:user:pw, bare host:user:pw, LPU; less precise output")
	noEncodingSniff := flag.Bool("no-encoding-sniff", false, "skip BOM detection; treat all inputs as UTF-8 (debug / A-B benchmark)")
	debug := flag.Bool("debug", false, "write structured job debug log in current working directory (CWD at start)")
	debugReject := flag.Bool("debug-reject", false, "append parser-rejected lines to a file in CWD")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable background update availability check")

	// allow positional anywhere on cmdline. flag.Parse stops at first
	// non-flag, so split first and pass only flag tokens
	flagArgs, positional := cliargs.SplitPositional(config.StripConfigArgv(os.Args[1:]), flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		// CommandLine defaults to ExitOnError, this branch only fires
		// if mode is ever switched. treat as usage failure
		reg.ExitWithCode(2)
	}
	visited := config.NewVisited()
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
		Debug: debug, DebugReject: debugReject,
	}); err != nil {
		fatalf("%v", err)
	}

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
			fmt.Fprintf(os.Stderr, "sfu: no input path provided; %s\n\n", cfgHint)
			flag.Usage()
			reg.ExitWithCode(2)
		}
		inputArg = cfgInput
	case 1:
		inputArg = positional[0]
	default:
		fmt.Fprintf(os.Stderr, "sfu: expected exactly one input path; got %d\n\n", len(positional))
		flag.Usage()
		reg.ExitWithCode(2)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fatalf("getwd: %v", err)
	}

	inputs, err := ulpengine.CollectInputs(inputArg)
	if err != nil {
		fatalf("input: %v", err)
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

	outDir, autoMkdir, err := resolveOutputDir(outFlagName, outArg)
	if err != nil {
		usagef("%v", err)
	}
	outDirAbs, err := filepath.Abs(outDir)
	if err != nil {
		fatalf("resolve output dir: %v", err)
	}
	base := ulpengine.DefaultBasename(stamp)

	// output path = dir + basename + optional .zst. 1 vs N parts decided
	// later by chunkedZstdSink. multi-part rename only fires when _part2
	// actually opens, so single-archive runs never carry _part suffix
	absOut, err := filepath.Abs(filepath.Join(outDirAbs, ulpengine.WithZstExt(base, *zst)))
	if err != nil {
		fatalf("resolve output: %v", err)
	}

	if autoMkdir && !dryRun {
		if err := os.MkdirAll(outDirAbs, 0o755); err != nil {
			fatalf("create output dir: %v", err)
		}
	}

	cfg := ulpengine.Config{
		Inputs:          inputs,
		Output:          absOut,
		TempDir:         *tempDir,
		Workers:         *workers,
		DedupWorkers:    *dedupW,
		Buckets:         *buckets,
		FastPathOff:     fileCfg.SFU.NoFastPath,
		Compress:        *zst,
		ZstChunkLines:   *splitZst,
		RunStarted:      started,
		RunStamp:        stamp,
		DeleteInputs:    *delSrc,
		NoURI:           *noURI,
		Loose:           *loose,
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

	var dbg *ulpengine.DebugLog
	var rr *ulpengine.RejectRecorder
	var debugLogPath string
	if *debug {
		p, err := ulpengine.DebugArtifactPath(cwd, "sfu-debug", ".log", stamp)
		if err != nil {
			fatalf("debug log path: %v", err)
		}
		debugLogPath = p
		dbg, err = ulpengine.NewDebugLog(p)
		if err != nil {
			fatalf("debug log: %v", err)
		}
		r.Cfg.Debug = dbg
	}
	if *debugReject {
		p, err := ulpengine.DebugArtifactPath(cwd, "sfu-rejected", ".txt", stamp)
		if err != nil {
			fatalf("debug-reject path: %v", err)
		}
		rr, err = ulpengine.NewRejectRecorder(p)
		if err != nil {
			fatalf("debug-reject: %v", err)
		}
		r.Cfg.Reject = rr
	}
	defer func() {
		if dbg != nil {
			_ = dbg.Close()
		}
		if rr != nil {
			_ = rr.Close()
		}
	}()

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

	// install handlers BEFORE preflight prompt so Ctrl-C at the prompt
	// exits 130 cleanly instead of swallowing the keystroke
	ctx, cancel, signaled := reg.SignalContext()
	defer cancel()

	ok, err := preflightCheck(ctx, r, isStdinTTY(os.Stdin), os.Stdin, os.Stderr)
	if err != nil {
		// ctx.Err at the prompt = user Ctrl-C'd. exit 130 not "preflight: ..."
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\ninterrupted")
			reg.ExitWithCode(130)
		}
		fatalf("preflight: %v", err)
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted by user")
		reg.ExitWithCode(2)
	}

	m := &ulpengine.Metrics{TotalInputBytes: r.TotalInputs}

	doneCh := make(chan struct{})
	tuiOff := *noTUI || !stderrIsCharDevice() || !vtOK

	var monitorWG sync.WaitGroup
	if !tuiOff {
		monitorWG.Add(1)
		go monitor(doneCh, started, m, r, signaled, &monitorWG)
	}

	runErr := ulpengine.Run(ctx, r, m)

	close(doneCh)

	// wait for monitor's deferred frame.close to leave alt-screen
	// cleanly before printing summary. replaces a race-y 50ms sleep
	if !tuiOff {
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
			ulpengine.PrintManualCleanupHint(os.Stderr)
			reg.ExitWithCode(130)
		}
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", runErr)
		reg.ExitWithCode(1)
	}

	if *delSrc && !dryRun {
		// delete BEFORE logCompletion so outcome lands in same block
		deleted, err := ulpengine.DeleteParsedInputs(inputs, r.OutputPaths)
		if err != nil {
			dbg.Event("del: FAILED after removing %d/%d input(s) err=%v", len(deleted), len(inputs), err)
			fmt.Fprintf(os.Stderr, "sfu: delete inputs: %v\n", err)
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
	tw := termWidth()
	// NoticeForSummary returns nil when the check is disabled, so no extra guard.
	updateNotice := updateChecker.NoticeForSummary()
	for _, ln := range renderFinalStdoutSummary(time.Since(started), m, r, tw, updateNotice) {
		fmt.Fprintln(os.Stderr, ln)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfu: "+format+"\n", args...)
	reg.ExitWithCode(1)
}

// argv-shape error, exit 2 (distinct from runtime 1)
func usagef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfu: "+format+"\n", args...)
	reg.ExitWithCode(2)
}

// live status loop. samples metrics every ~300ms, computes per-tick
// rates, draws an 80-col block. signaled=true swaps to INTERRUPTED
// frame. wg.Done fires after frame.close so callers sync on clean exit
func monitor(done <-chan struct{}, started time.Time, m *ulpengine.Metrics, r *ulpengine.Resolved, signaled func() bool, wg *sync.WaitGroup) {
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
		w := termWidth()

		// interrupt overrides phase layout for the rest of process
		// lifetime. prev* not updated, rates meaningless in shutdown
		if signaled != nil && signaled() {
			frame.draw(renderInterruptLines(elapsed, w, ulpengine.SnapshotCleanupLog()))
			return
		}

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
		prevWritten = wr
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
