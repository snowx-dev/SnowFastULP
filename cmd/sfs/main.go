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
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

func main() {
	// VT on for windows ANSI, no-op on unix. must run pre-output
	console.EnableVT()

	flag.Usage = func() { printHelp(filepath.Base(os.Args[0]), os.Stderr) }

	if cliargs.IsVersionRequest(os.Args[1:]) {
		fmt.Printf("SnowFastSearch %s\n", version.String)
		return
	}
	if cliargs.IsHelpRequest(os.Args[1:]) {
		printHelp(filepath.Base(os.Args[0]), os.Stdout)
		os.Exit(0)
	}

	cfg, err := config.LoadFromArgv(os.Args[1:])
	if err != nil {
		fatal("%v", err)
	}

	outFile := flag.String("o", "", "write results to this file (default: stdout)")
	txtMode := flag.Bool("txt", false, "search plain .txt files instead of .zst archives (no index)")
	silent := flag.Bool("silent", false, "disable progress UI")
	clean := flag.Bool("clean", false, "strip URL scheme prefixes from output lines")
	workers := flag.Int("j", 0, "worker goroutines (0 = GOMAXPROCS)")
	debugFlag := flag.Bool("debug", false, "write structured job debug log in current working directory (CWD at start)")
	// 1 MiB default tracks per-core L2 on modern uarch.
	// drop to 256 KiB on Broadwell/Haswell Xeon, Zen 2/3
	decodeStep := flag.Int("decode-step", 0, "")
	// per-chunk safety valve vs pathological queries (eg `:` over multi-GiB).
	// 0 = unbounded, hit = skip rest of chunk + stderr note
	maxHitsPerChunk := flag.Int("max-hits-per-chunk", 0, "")

	flagArgs, positional := cliargs.SplitPositional(config.StripConfigArgv(os.Args[1:]), flag.CommandLine)
	if err := flag.CommandLine.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	visited := config.NewVisited()
	if err := cfg.ApplySFS(visited, config.SFSFlags{
		O: outFile, Txt: txtMode, Silent: silent, Clean: clean, J: workers, Debug: debugFlag,
		DecodeStep: decodeStep, MaxHitsPerChunk: maxHitsPerChunk,
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
	if pattern == "" {
		fatal("empty pattern")
	}

	w := *workers
	if w <= 0 {
		w = runtime.GOMAXPROCS(0)
	}

	var archives []string
	if *txtMode {
		archives, err = discover.ListTxt(args.Root)
	} else {
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

	// dont clobber search target w/ -o, O_TRUNC fires pre-scan
	if err := ensureNoOutputCollision(*outFile, archives); err != nil {
		fatal("%v", err)
	}

	started := time.Now()
	ctx, cancel, signaled := signalContext()
	defer cancel()

	files := &fileabort.Registry{}
	ctx = fileabort.WithContext(ctx, files)
	go watchInterrupt(ctx, files, signaled)

	cwd, err := os.Getwd()
	if err != nil && *debugFlag {
		fatal("getwd: %v", err)
	}
	uiMode := resolveUIMode(*silent, *outFile)

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
		silent:     *silent,
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
		archives:        archives,
		txtMode:         *txtMode,
		workers:         w,
		outFile:         *outFile,
		silent:          *silent,
		clean:           *clean,
		decodeStep:      *decodeStep,
		maxHitsPerChunk: *maxHitsPerChunk,
		signaled:        signaled,
		started:         started,
		debug:           dbg,
		metrics:         metrics,
	})
	wall := time.Since(started)

	if runErr != nil {
		if dbg != nil {
			dbg.logTermination(runErr, signaled(), wall, metrics)
		}
		if signaled() {
			fmt.Fprintln(os.Stderr, "\ninterrupted")
			exitWithCode(130)
		}
		fatal("%v", runErr)
	}
	if dbg != nil {
		dbg.logCompletion(metrics, wall, debugInfo)
	}
	if !*silent {
		for _, ln := range renderFinalSummary(started, metrics, *outFile) {
			fmt.Fprintln(os.Stderr, ln)
		}
	}
}

type runConfig struct {
	root            string
	pattern         string
	archives        []string
	txtMode         bool
	workers         int
	outFile         string
	silent          bool
	clean           bool
	decodeStep      int
	maxHitsPerChunk int
	signaled        func() bool
	started         time.Time
	debug           *debugLog
	metrics         *search.Metrics
}

func run(ctx context.Context, cfg runConfig) error {
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

	var resultWriter io.Writer = os.Stdout
	if out != nil {
		resultWriter = out
	}

	uiMode := resolveUIMode(cfg.silent, cfg.outFile)
	var layout *terminalLayout
	if uiMode == uiFull && stdoutIsTTY() && stderrIsTTY() {
		layout = &terminalLayout{}
		layout.Enable()
	}
	if layout != nil {
		resultWriter = newHitViewportWriter(resultWriter, layout)
	}

	var capture *hitCapture
	if layout != nil {
		capture = newHitCapture(resultWriter)
		resultWriter = capture
	}

	metrics := cfg.metrics
	if metrics == nil {
		metrics = &search.Metrics{}
	}
	metrics.ArchivesTotal.Store(int64(len(cfg.archives)))

	var indexBytesTotal int64
	for _, arch := range cfg.archives {
		if cfg.txtMode {
			if st, err := os.Stat(arch); err == nil {
				indexBytesTotal += st.Size()
			}
		} else if sz, err := index.ArchiveSize(arch); err == nil {
			indexBytesTotal += sz
		}
	}
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

	uiDone := make(chan struct{})
	var uiWG sync.WaitGroup
	uiWG.Add(1)
	go runUI(uiConfig{
		Mode:     uiMode,
		Metrics:  metrics,
		Start:    cfg.started,
		Done:     uiDone,
		Signaled: cfg.signaled,
		Layout:   layout,
	}, &uiWG)
	ok := false
	defer func() {
		close(uiDone)
		uiWG.Wait()
		if ok && capture != nil {
			capture.ReplayToStdout()
		}
	}()

	var sidecars map[string]*index.Sidecar
	if !cfg.txtMode {
		var err error
		sidecars, err = indexArchives(ctx, cfg.archives, cfg.workers, metrics, cfg.debug)
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
	streamFlush := layout != nil || (out == nil && stdoutIsTTY())

	var printer *search.OrderedPrinter
	writeHit := func(h search.Hit) error {
		return sink.WriteHit(h)
	}
	if orderedOutput {
		printer = search.NewOrderedPrinter(func(h search.Hit) error {
			return sink.WriteHit(h)
		})
		writeHit = printer.Add
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
				Pattern:    []byte(cfg.pattern),
				Workers:    cfg.workers,
				Files:      cfg.archives,
				Metrics:    metrics,
				Hits:       hitCh,
				ArchiveOrd: archiveOrd,
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
				Pattern:         []byte(cfg.pattern),
				Workers:         cfg.workers,
				Archives:        cfg.archives,
				Sidecars:        sidecars,
				Metrics:         metrics,
				Hits:            hitCh,
				ArchiveOrd:      archiveOrd,
				OnChunkError: func(archive string, chunkID int, err error) {
					if cfg.debug != nil {
						cfg.debug.Event("chunk error archive=%s chunk=%d err=%v", filepath.Base(archive), chunkID, err)
					}
				},
				OnChunkCapped: func(archive string, chunkID int, emitted int) {
					fmt.Fprintf(os.Stderr, "note: %s chunk %d: hit cap reached (%d hits); chunk truncated\n",
						filepath.Base(archive), chunkID, emitted)
					if cfg.debug != nil {
						cfg.debug.Event("chunk capped archive=%s chunk=%d emitted=%d", filepath.Base(archive), chunkID, emitted)
					}
				},
			})
		}
		close(hitCh)
	}()

drainHits:
	for {
		select {
		case <-ctx.Done():
			break drainHits
		case h, ok := <-hitCh:
			if !ok {
				break drainHits
			}
			firstHit.Do(func() {
				if cfg.debug != nil {
					cfg.debug.Event("first hit archive=%s chunk=%d offset=%d", filepath.Base(h.Archive), h.ChunkID, h.Offset)
				}
			})
			if err := writeHit(h); err != nil {
				return fmt.Errorf("write hit: %w", err)
			}
			if streamFlush {
				if err := sink.Flush(); err != nil {
					return fmt.Errorf("flush hit: %w", err)
				}
			}
		}
	}

	searchWG.Wait()
	if cfg.debug != nil {
		cfg.debug.Event("search complete hits=%d chunks=%d/%d scanned=%d",
			metrics.Hits.Load(), metrics.ChunksDone.Load(), metrics.ChunksTotal.Load(), metrics.BytesScanned.Load())
	}
	if ctx.Err() != nil {
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
	if searchErr != nil {
		return searchErr
	}
	ok = true
	return nil
}

func indexArchives(ctx context.Context, archives []string, workers int, metrics *search.Metrics, dbg *debugLog) (map[string]*index.Sidecar, error) {
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
				act := indexActivity(metrics, filepath.Base(arch))
				sc, meta, err := index.Ensure(ctx, arch, progress.Callback(), act)
				if err != nil {
					metrics.IndexArchivesActive.Add(-1)
					if ctx.Err() != nil {
						return
					}
					if dbg != nil {
						dbg.Event("index failed archive=%s err=%v", filepath.Base(arch), err)
					}
					fmt.Fprintf(os.Stderr, "index %s: %v\n", arch, err)
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
	exitWithCode(1)
}

// argv-shape error, exits 2 vs runtime errors (1) so scripts can branch
func usage(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sfs: "+format+"\n", args...)
	exitWithCode(2)
}
