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
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/console"
	"github.com/snowx-dev/SnowFastULP/internal/discover"
	"github.com/snowx-dev/SnowFastULP/internal/fdlimit"
	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

// reg is the shared terminal restore/exit registry for the process: the live
// screen registers its teardown via Set, and every exit path (graceful
// ExitWithCode, force-exit on a second Ctrl-C, cleanup timeout) routes
// through it so the alt-screen is always left cleanly. nil cleanupHint: sfs
// has no manual-cleanup hint (unlike sfu/sfl).
var reg = termctl.New(os.Stderr, nil)

func main() {
	// VT on for windows ANSI, no-op on unix. must run pre-output. vtOK is false
	// only on a legacy console that can't render ANSI, forcing the silent UI so
	// escapes never leak as raw text.
	vtOK := console.EnableVT()

	flag.Usage = func() { printHelp(filepath.Base(os.Args[0]), os.Stderr) }

	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastSearch %s\n", version.String)
		return
	}
	if cliargs.IsHelpRequest(os.Args[1:]) {
		printHelp(filepath.Base(os.Args[0]), os.Stdout)
		os.Exit(0)
	}

	// `update` / `upgrade`: replace installed SnowFast binaries with the latest release.
	// Handled before cfg load so a bad config can't block self-update.
	if handled, err := selfupdate.Dispatch(os.Args[1:], version.String, os.Stdout); handled {
		if err != nil {
			fatal("%v", err)
		}
		return
	}

	// Gate color on stderr (the live-screen target): a redirected stderr must
	// never accumulate ANSI escapes even when stdout is a TTY.
	applyStderrColorProfile()

	cfg, err := config.LoadFromArgv(os.Args[1:])
	if err != nil {
		fatal("%v", err)
	}

	outFile := flag.String("o", "", "write results to this file (default: sfs_results_YYYYMMDD-HHMM.txt)")
	stream := flag.Bool("s", false, "stream results to stdout without the live screen")
	txtMode := flag.Bool("txt", false, "search plain .txt files instead of .zst archives (no index)")
	silent := flag.Bool("silent", false, "deprecated alias for -s")
	clean := flag.Bool("clean", false, "strip URL scheme prefixes from output lines")
	since := flag.String("since", "", "only search archives modified within this window, e.g. 7d, 12h, 90m (default: all)")
	workers := flag.Int("j", 0, "worker goroutines (0 = GOMAXPROCS)")
	workersAlias := flag.Int("workers", 0, "alias for -j")
	debugFlag := flag.Bool("debug", false, "write structured job debug log in current working directory (CWD at start)")
	noUpdateCheck := flag.Bool("no-update-check", false, "disable background update availability check")
	// 1 MiB default matches the search engine default; tune only after profiling.
	// zst-only: -txt reads use a fixed 1 MiB step and ignore this flag.
	decodeStep := flag.Int("decode-step", 0, "zst only: bytes per decoder read (0 = 1 MiB default; ignored in -txt mode)")
	// per-chunk safety valve vs pathological queries (eg `:` over multi-GiB).
	// 0 = unbounded, hit = skip rest of chunk + stderr note
	maxHitsPerChunk := flag.Int("max-hits-per-chunk", 0, "")
	// global hit cap: stop the whole search + exit cleanly after N total hits.
	// 0 = unlimited. distinct from -max-hits-per-chunk (per-chunk safety valve).
	limit := flag.Int("l", 0, "stop after N total hits, then exit (0 = unlimited)")
	// -sec switches to the secrets DB (written by `sfl -secrets`): the PATTERN
	// filters by secret type (rule id/name, "*" = all) instead of a line match.
	sec := flag.Bool("sec", false, "search the secrets DB instead of ULP archives")
	secretsPath := flag.String("secrets-path", "", "path to the secrets DB (default: <root>/sfl-secrets.sqlite)")
	secretsPathAlias := flag.String("sec-path", "", "alias for -secrets-path")

	flagArgs, positional := cliargs.SplitPositional(config.StripConfigArgv(os.Args[1:]), flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	visited := config.NewVisited()
	// Accept -workers as an alias for -j (sfu/sfl use -workers) so the same
	// invocation works across all three CLIs; explicit -j wins.
	visited.ResolveIntAlias(workers, workersAlias, "j", "workers")
	visited.ResolveStringAlias(secretsPath, secretsPathAlias, "secrets-path", "sec-path")
	if err := cfg.ApplySFS(visited, config.SFSFlags{
		O: outFile, Txt: txtMode, Stream: stream, Silent: silent, Clean: clean, J: workers, Debug: debugFlag,
		DecodeStep: decodeStep, MaxHitsPerChunk: maxHitsPerChunk, Limit: limit, Since: since,
		Sec: sec, SecretsPath: secretsPath,
	}); err != nil {
		fatal("%v", err)
	}

	args, err := parseSearchArgs(positional)
	if err != nil {
		flag.Usage()
		usage("%v", err)
	}
	if len(positional) == 1 && cfg.SFS.Dir != "" {
		dir, err := cfg.ResolvedSFSDir()
		if err != nil {
			fatal("%v", err)
		}
		args.Root = dir
	}
	pattern := args.Pattern
	matchAll := pattern == "*"
	if pattern == "" {
		fatal("empty pattern")
	}
	// -sec is a distinct, self-contained mode: it reads the secrets DB (no
	// archive discovery, indexing, or live search), so branch out here before
	// any of that machinery spins up.
	if *sec {
		if err := runSecretsSearch(secretsSearchArgs{
			root:        args.Root,
			pattern:     pattern,
			secretsPath: *secretsPath,
			since:       *since,
			limit:       *limit,
			outFile:     *outFile,
			clean:       *clean,
		}); err != nil {
			fatal("%v", err)
		}
		return
	}
	if matchAll && *limit == 0 && *since == "" {
		fmt.Fprintln(os.Stderr, "note: '*' exports all lines; use -l N or -since DUR to narrow scope")
	}

	w := *workers
	if w <= 0 {
		w = runtime.GOMAXPROCS(0)
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
		fatal("%v", err)
	}

	// fd preflight, worst case ~2*W + sidecars + stdio.
	// clamp W to fit RLIMIT_NOFILE so we dont EMFILE mid-run.
	// best-effort, keep w if query fails
	if maxFD, ok := fdlimit.MaxOpenFiles(); ok && maxFD > 0 {
		const fdReserve = 16 // stdio + sidecars + decoder slots
		safeW := (maxFD - fdReserve) / 2
		if safeW < 1 {
			safeW = 1
		}
		if safeW < w {
			fmt.Fprintf(os.Stderr, "note: clamping -j from %d to %d to fit RLIMIT_NOFILE=%d\n", w, safeW, maxFD)
			w = safeW
		}
	}

	// -txt mode warns if .zst archives lurk in same root, inverse stays silent
	if *txtMode {
		if zstFiles, zerr := discover.ListZst(args.Root); zerr == nil && len(zstFiles) > 0 {
			fmt.Fprintf(os.Stderr, "note: -txt mode; %d .zst archive(s) under %s ignored\n", len(zstFiles), args.Root)
		}
	}

	started := time.Now()
	cwd, err := os.Getwd()
	if err != nil {
		fatal("getwd: %v", err)
	}
	streamMode := streamRequested(*stream, *silent)
	outputMode, err := resolveOutputMode(*outFile, streamMode, cwd, started)
	if err != nil {
		fatal("%v", err)
	}
	*outFile = outputMode.OutFile
	streamMode = outputMode.Stream
	// dont clobber search target w/ -o/default output, O_TRUNC fires pre-scan
	if err := ensureNoOutputCollision(*outFile, archives); err != nil {
		fatal("%v", err)
	}

	updateChecker := selfupdate.NewChecker(version.String, os.Args[0], *noUpdateCheck)
	updateChecker.Start()
	ctx, cancel, signaled := reg.SignalContext()
	defer cancel()

	files := &fileabort.Registry{}
	ctx = fileabort.WithContext(ctx, files)
	go reg.WatchInterrupt(ctx, files, signaled)

	uiMode := resolveUIMode(streamMode || !vtOK)

	var dbg *debugLog
	var debugLogPath string
	if *debugFlag {
		if cwd == "" {
			cwd, err = os.Getwd()
			if err != nil {
				fatal("getwd: %v", err)
			}
		}
		p, err := debugArtifactPath(cwd, "sfs-debug", ".log", debugStamp(started))
		if err != nil {
			fatal("debug log path: %v", err)
		}
		debugLogPath = p
		dbg, err = newDebugLog(p)
		if err != nil {
			fatal("debug log: %v", err)
		}
		defer func() { _ = dbg.Close() }()
	}

	debugInfo := debugRunInfo{
		root:       args.Root,
		pattern:    pattern,
		patternLen: len(pattern),
		workers:    w,
		outFile:    *outFile,
		stream:     streamMode,
		clean:      *clean,
		cwd:        cwd,
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
		decodeStep:      *decodeStep,
		maxHitsPerChunk: *maxHitsPerChunk,
		limit:           *limit,
		signaled:        signaled,
		started:         started,
		debug:           dbg,
		metrics:         metrics,
		indexBytesTotal: debugInfo.indexBytesTotal,
		uiMode:          uiMode,
	})
	wall := time.Since(started)

	if runErr != nil {
		if dbg != nil {
			dbg.logTermination(runErr, signaled(), wall, metrics)
		}
		if signaled() {
			fmt.Fprintln(os.Stderr, "\ninterrupted")
			reg.ExitWithCode(130)
		}
		fatal("%v", runErr)
	}
	if dbg != nil {
		dbg.logCompletion(metrics, wall, debugInfo)
	}
	if !streamMode {
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
	decodeStep      int
	maxHitsPerChunk int
	limit           int
	signaled        func() bool
	started         time.Time
	debug           *debugLog
	metrics         *search.Metrics
	// indexBytesTotal and uiMode are resolved by the caller (main) since it
	// already computes them for the debug header; run() consumes them instead
	// of re-statting every archive and re-resolving the UI mode.
	indexBytesTotal int64
	uiMode          uiMode
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

	var out *os.File
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

	uiMode := cfg.uiMode

	// Hit output sink: a file when -o / default-generated, otherwise stdout for
	// -s streaming. The live status frame (stderr) shows a hit counter and
	// progress; results themselves are never painted into the alt-screen, which
	// is what kept letting the frame leak onto the user's scrollback.
	var resultWriter io.Writer = os.Stdout
	if out != nil {
		resultWriter = out
	}

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
	if !cfg.txtMode {
		var err error
		sidecars, err = indexArchives(ctx, cfg.archives, cfg.workers, metrics, cfg.debug, addNote)
		if err != nil {
			return err
		}
		if len(sidecars) == 0 {
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
	sink := search.NewWriter(resultWriter, cfg.clean)
	orderedOutput := cfg.outFile != ""
	// Flush each hit only when streaming to an interactive stdout (-s on a TTY)
	// so the user sees results live; piped/file runs buffer for throughput.
	streamFlush := out == nil && stdoutIsTTY()

	var printer *search.OrderedPrinter
	writeHit := func(h search.Hit) error {
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
		printer = search.NewOrderedPrinter(func(h search.Hit) error {
			return sink.WriteHit(h)
		})
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
				Ctx:        ctx,
				MatchAll:   cfg.matchAll,
				Pattern:    []byte(cfg.pattern),
				Workers:    cfg.workers,
				Files:      cfg.archives,
				Metrics:    metrics,
				Hits:       hitCh,
				ArchiveOrd: archiveOrd,
				OnFileDone: onArchiveDone,
				OnFileError: func(path string, err error) {
					if cfg.debug != nil {
						cfg.debug.Event("file error path=%s err=%v", filepath.Base(path), err)
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
		if ctx.Err() != nil && !limitReached && cfg.outFile != "" {
			discardInterruptedOutput(cfg.outFile, out)
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
	if searchErr != nil && !limitReached {
		return searchErr
	}
	return nil
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

func indexArchives(ctx context.Context, archives []string, workers int, metrics *search.Metrics, dbg *debugLog, note func(string)) (map[string]*index.Sidecar, error) {
	sidecars := make(map[string]*index.Sidecar, len(archives))
	var sidecarMu sync.Mutex
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
					note(fmt.Sprintf("index %s: %v", arch, err))
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
		return sidecars, ctx.Err()
	}
	return sidecars, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfs: "+format+"\n", args...)
	reg.ExitWithCode(1)
}

// argv-shape error, exits 2 vs runtime errors (1) so scripts can branch
func usage(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfs: "+format+"\n", args...)
	reg.ExitWithCode(2)
}
