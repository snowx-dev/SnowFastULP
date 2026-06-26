package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/console"
	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
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
	w := *workers
	if w <= 0 {
		w = runtime.GOMAXPROCS(0)
		if w > 8 {
			w = 8
		}
		if w < 1 {
			w = 1
		}
	}

	cfg := runConfig{
		Input: inputArg, OutputDir: *out, LibraryDir: *outDedup, Password: *password,
		TempDir: *tempDir, Workers: w, Compress: *zst, DeleteSources: *delSrc,
		NoURI: *noURI, NoTUI: *noTUI, Debug: *debug, NoUpdateCheck: *noUpdateCheck,
		Started: started,
	}
	if err := run(cfg); err != nil {
		fatalf("%v", err)
	}
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
// (classic file or, for -od, a temp ULP fed to sfu), then optionally deletes
// parsed sources and prints the summary. The monitor is always torn down before
// any further stderr output so frames never interleave.
func run(cfg runConfig) error {
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

	prog := sflog.NewProgress()
	tuiOff := cfg.NoTUI || !stdoutIsCharDevice()
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

	eng := buildEngine(cfg, passwords, prog, dbg)
	stats, results, extractErr := eng.Run(ctx, cfg.Input, snk.w)
	finalizeErr := snk.finalize(extractErr != nil)

	stopMonitor()

	if extractErr != nil {
		snk.cleanup()
		if signaled() {
			printInterruptSummary(cfg)
			exitWithCode(130)
		}
		return extractErr
	}
	if finalizeErr != nil {
		snk.cleanup()
		return finalizeErr
	}

	outPath := snk.outPath
	if cfg.LibraryDir != "" {
		if stats.Emitted == 0 {
			snk.cleanup()
			return fmt.Errorf("no credentials extracted from %s", cfg.Input)
		}
		// Hand the screen to sfu: it renders its own merge TUI and the
		// authoritative library summary, so sfl stays quiet through ingestion.
		if err := ingestToLibrary(ctx, cfg, snk.ulpPath); err != nil {
			snk.cleanup()
			if signaled() {
				// sfu already printed its own interrupt notice; don't double up.
				exitWithCode(130)
			}
			return err
		}
	} else if stats.Emitted == 0 {
		// L5: never leave an empty output file behind.
		_ = os.Remove(snk.outPath)
		outPath = "(no ULP detected)"
	}
	snk.cleanup()

	if cfg.DeleteSources {
		deleted, err := deleteParsedSources(cfg.Input, results, snk.protected)
		if err != nil {
			return fmt.Errorf("delete sources: %w", err)
		}
		dbg.Event("del: removed %d source unit(s)", len(deleted))
	}

	// -od ingestion is summarised by sfu itself; only classic (-o) runs print
	// sfl's own final summary so the two don't duplicate.
	if cfg.LibraryDir == "" {
		for _, ln := range renderFinalSummary(outPath, stats) {
			fmt.Fprintln(os.Stderr, ln)
		}
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

func buildEngine(cfg runConfig, passwords []string, prog *sflog.Progress, dbg *debugLogger) *sflog.Engine {
	eng := &sflog.Engine{
		Workers:   cfg.Workers,
		NoURI:     cfg.NoURI,
		Passwords: passwords,
		Progress:  prog,
	}
	if dbg != nil {
		eng.Debug = dbg.Event
	}
	return eng
}

func ingestToLibrary(ctx context.Context, cfg runConfig, ulpPath string) error {
	if err := os.MkdirAll(cfg.LibraryDir, 0o755); err != nil {
		return err
	}
	sfu, err := locateSFUBinary()
	if err != nil {
		return err
	}
	// Let sfu drive its own merge TUI; only force plain output when sfl itself
	// was asked to run without a TUI. (sfu also auto-disables on a non-TTY.)
	args := []string{ulpPath, "-od", cfg.LibraryDir}
	if cfg.NoTUI {
		args = append(args, "-no-tui")
	}
	if cfg.NoURI {
		args = append(args, "-no-uri")
	}
	if cfg.Workers > 0 {
		args = append(args, "-workers", fmt.Sprintf("%d", cfg.Workers))
	}
	if cfg.TempDir != "" {
		args = append(args, "-temp-dir", cfg.TempDir)
	}
	if cfg.NoUpdateCheck {
		args = append(args, "-no-update-check")
	}
	cmd := exec.CommandContext(ctx, sfu, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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

func locateSFUBinary() (string, error) {
	if p := strings.TrimSpace(os.Getenv("SFL_SFU_BIN")); p != "" {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		name := "sfu"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("sfu"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("sfu binary not found; install sfu next to sfl or set SFL_SFU_BIN")
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
