package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// registerFlags mirrors onto fs the full set of sfu CLI flags registered in
// main(). Tests never run main(), but SplitPositional and other helpers
// consult the flag set, so the flags must exist on flag.CommandLine during
// tests. Keep this list in sync with main.go's registrations.
func registerFlags(fs *flag.FlagSet) {
	if fs.Lookup("o") != nil {
		return
	}
	fs.String("o", "", "")
	fs.String("od", "", "")
	fs.Int("workers", 0, "")
	fs.Int("j", 0, "")
	fs.Int("dedup", 0, "")
	fs.Int("buckets", 0, "")
	fs.String("temp-dir", "", "")
	fs.Bool("no-tui", false, "")
	fs.Bool("zst", false, "")
	fs.Int64("split-zst", 0, "")
	fs.Bool("del", false, "")
	fs.Bool("no-uri", false, "")
	fs.Bool("loose", false, "")
	fs.Bool("no-encoding-sniff", false, "")
	fs.Bool("debug", false, "")
	fs.Bool("debug-reject", false, "")
	fs.Bool("no-update-check", false, "")
	fs.Bool("history", false, "")
	fs.String("history-path", "", "")
}

func init() {
	registerFlags(flag.CommandLine)
}

// TestMain sandboxes the whole package's temp directory, mirroring cmd/sfl's
// TestMain: the e2e helpers build the sfu binary into an os.MkdirTemp dir and
// spawn children whose spill dirs land in TMPDIR, and the force-exit e2e
// SIGINTs/SIGKILLs those children so they cannot clean up after themselves —
// cleanup belongs to the TEST process, not the child. Without the sandbox
// those snowfast-sfu-e2e-* build dirs and child spill dirs leak into the
// user's real temp root on every suite run. Individual tests may still narrow
// it further with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sfu-test-tmp")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sfu test setup:", err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// every documented --version invocation wins, plain inputs lose.
// whole-argv scan means `./input.txt --version` also wins now
func TestIsVersionRequestSfuArgvShapes(t *testing.T) {
	for _, argv := range [][]string{
		{"--version"},
		{"-version"},
		{"version"},
		{"./input.txt", "--version"},
	} {
		if !cliargs.IsVersionRequest(argv) {
			t.Fatalf("IsVersionRequest(%v) = false, want true", argv)
		}
	}
	for _, argv := range [][]string{
		nil,
		{"./input.txt"},
		{"-no-tui", "./input.txt"},
	} {
		if cliargs.IsVersionRequest(argv) {
			t.Fatalf("IsVersionRequest(%v) = true, want false", argv)
		}
	}
}

func TestEnsureDestDedupMetricsPrePublishesPointers(t *testing.T) {
	r := &ulpengine.Resolved{Cfg: ulpengine.Config{DestDedup: true}}
	ulpengine.EnsureDestDedupMetrics(r)
	if r.OdMetrics == nil {
		t.Fatal("odMetrics was not initialized")
	}
	od := r.OdMetrics
	ulpengine.EnsureDestDedupMetrics(r)
	if r.OdMetrics != od {
		t.Fatal("ensureDestDedupMetrics replaced already-published metric pointers")
	}
}

func TestSplitPositionalSplitZstPairsValue(t *testing.T) {
	flags, pos := cliargs.SplitPositional([]string{"-split-zst", "42", "./in.txt"}, flag.CommandLine)
	if len(pos) != 1 || pos[0] != "./in.txt" {
		t.Fatalf("pos = %#v", pos)
	}
	var haveFlag, haveVal bool
	for i := 0; i < len(flags); i++ {
		if flags[i] == "-split-zst" {
			haveFlag = true
			if i+1 < len(flags) && flags[i+1] == "42" {
				haveVal = true
			}
		}
	}
	if !haveFlag || !haveVal {
		t.Fatalf("flags = %#v", flags)
	}
}

func TestHistoryFlagsAroundPositionals(t *testing.T) {
	tests := []struct {
		name        string
		argv        []string
		wantHistory string
		wantPath    string
	}{
		{name: "enabled after positional", argv: []string{"./in.txt", "--history"}, wantHistory: "true"},
		{name: "explicit false before positional", argv: []string{"--history=false", "./in.txt"}, wantHistory: "false"},
		{name: "path pair after positional", argv: []string{"./in.txt", "--history-path", "history.sqlite3"}, wantHistory: "false", wantPath: "history.sqlite3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("history", flag.ContinueOnError)
			registerFlags(fs)
			flags, positional := cliargs.SplitPositional(tc.argv, fs)
			if len(positional) != 1 || positional[0] != "./in.txt" {
				t.Fatalf("positionals = %#v, want [./in.txt]", positional)
			}
			if err := fs.Parse(flags); err != nil {
				t.Fatalf("parse flags %#v: %v", flags, err)
			}
			if got := fs.Lookup("history").Value.String(); got != tc.wantHistory {
				t.Fatalf("history = %q, want %q", got, tc.wantHistory)
			}
			if got := fs.Lookup("history-path").Value.String(); got != tc.wantPath {
				t.Fatalf("history-path = %q, want %q", got, tc.wantPath)
			}
		})
	}
}

func TestDeleteParsedInputsRemovesAndSkipsOutputPaths(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "a.txt")
	outKeep := filepath.Join(d, "sfu_20260101-120000_part1.txt.zst")
	if err := os.WriteFile(in, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outKeep, []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	// path listed as both input and output must not be deleted
	deleted, err := ulpengine.DeleteParsedInputs([]string{in, outKeep}, []string{outKeep})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v, want 1 path", deleted)
	}
	if _, err := os.Stat(in); !os.IsNotExist(err) {
		t.Fatalf("input should be removed: %v", err)
	}
	if _, err := os.Stat(outKeep); err != nil {
		t.Fatalf("path matching output set should be skipped: %v", err)
	}
}

// -odr is -od's dry-run twin: mutually exclusive with -o and -od, implies
// dedup+dry-run, requires a non-empty path, and config odr=true reuses the
// od path (erroring without one).
func TestResolveOutputMode(t *testing.T) {
	tests := []struct {
		name                        string
		out, outDedup, outDryRun    string
		odPassed, odrPassed, odrCfg bool
		wantErr                     string
		wantDestDedup, wantDryRun   bool
		wantOutArg, wantFlag        string
	}{
		{
			name: "plain -o", out: "./out/", odrCfg: false,
			wantDestDedup: false, wantDryRun: false, wantOutArg: "./out/", wantFlag: "-o",
		},
		{
			name: "-od dedup", outDedup: "./lib/", odPassed: true,
			wantDestDedup: true, wantDryRun: false, wantOutArg: "./lib/", wantFlag: "-od",
		},
		{
			name: "-odr dry-run", outDryRun: "./lib/", odrPassed: true,
			wantDestDedup: true, wantDryRun: true, wantOutArg: "./lib/", wantFlag: "-odr",
		},
		{
			name: "-o and -od exclusive", out: "./out/", outDedup: "./lib/",
			wantErr: "mutually exclusive",
		},
		{
			name: "-o and -odr exclusive", out: "./out/", outDryRun: "./lib/",
			wantErr: "mutually exclusive",
		},
		{
			name: "-od and -odr exclusive", outDedup: "./lib/", outDryRun: "./lib2/",
			wantErr: "mutually exclusive",
		},
		{
			name: "empty -odr rejected", outDryRun: "", odrPassed: true,
			wantErr: "-odr requires a directory path",
		},
		{
			name: "empty -od rejected", outDedup: "", odPassed: true,
			wantErr: "-od requires a directory path",
		},
		{
			name: "config odr reuses od path", outDedup: "./lib/", odrCfg: true,
			wantDestDedup: true, wantDryRun: true, wantOutArg: "./lib/", wantFlag: "-odr",
		},
		{
			name: "config odr without od errors", odrCfg: true,
			wantErr: "odr=true requires a library path",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := resolveOutputMode(tc.out, tc.outDedup, tc.outDryRun, tc.odPassed, tc.odrPassed, tc.odrCfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.destDedup != tc.wantDestDedup {
				t.Errorf("destDedup = %v, want %v", m.destDedup, tc.wantDestDedup)
			}
			if m.dryRun != tc.wantDryRun {
				t.Errorf("dryRun = %v, want %v", m.dryRun, tc.wantDryRun)
			}
			if m.outArg != tc.wantOutArg {
				t.Errorf("outArg = %q, want %q", m.outArg, tc.wantOutArg)
			}
			if m.outFlagName != tc.wantFlag {
				t.Errorf("outFlagName = %q, want %q", m.outFlagName, tc.wantFlag)
			}
		})
	}
}
