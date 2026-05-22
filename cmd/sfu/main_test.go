package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
)

func init() {
	// SplitPositional consults the flag set, prod registers in main()
	// which tests never run. mirror registrations here
	if flag.CommandLine.Lookup("o") != nil {
		return
	}
	flag.String("o", "", "")
	flag.Int("workers", 0, "")
	flag.Int("dedup", 0, "")
	flag.Int("buckets", 0, "")
	flag.String("temp-dir", "", "")
	flag.Bool("no-tui", false, "")
	flag.Bool("zst", false, "")
	flag.Int64("split-zst", 0, "")
	flag.Bool("del", false, "")
	flag.Bool("no-uri", false, "")
	flag.Bool("debug", false, "")
	flag.Bool("debug-reject", false, "")
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
	r := &resolved{cfg: pipelineConfig{DestDedup: true}}
	ensureDestDedupMetrics(r)
	if r.odMetrics == nil {
		t.Fatal("odMetrics was not initialized")
	}
	if r.outputIdxMetrics == nil {
		t.Fatal("outputIdxMetrics was not initialized")
	}
	od := r.odMetrics
	out := r.outputIdxMetrics
	ensureDestDedupMetrics(r)
	if r.odMetrics != od || r.outputIdxMetrics != out {
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
	deleted, err := deleteParsedInputs([]string{in, outKeep}, []string{outKeep})
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
