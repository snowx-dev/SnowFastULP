package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	"github.com/snowx-dev/SnowFastULP/internal/history"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// decodeJSONL decodes every non-empty line of an NDJSON string.
func decodeJSONL(t *testing.T, data string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(data), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", ln, err)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		t.Fatal("json stream is empty")
	}
	return out
}

// readJSONL decodes every non-empty line of an NDJSON file into raw objects.
func readJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read json stream: %v", err)
	}
	return decodeJSONL(t, string(data))
}

// A non-positive -json-every is a usage error when the stream is on, and
// inert while the stream is off.
func TestRunJSONOutRejectsNonPositiveEvery(t *testing.T) {
	prog := sflog.NewProgress()
	path := filepath.Join(t.TempDir(), "stats.jsonl")
	for _, every := range []time.Duration{0, -time.Second} {
		j, err := newJSONOut(runConfig{JSONOut: path, JSONEvery: every}, prog, nil)
		if err == nil || !strings.Contains(err.Error(), "must be positive") {
			t.Fatalf("every=%v: err=%v, want a positivity error", every, err)
		}
		if j != nil {
			t.Fatalf("every=%v: stream created despite error", every)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("every=%v: stream file created despite error", every)
		}
	}
	if j, err := newJSONOut(runConfig{JSONEvery: 0}, prog, nil); err != nil || j.em != nil {
		t.Fatalf("stream off + every=0: err=%v em=%v, want a no-op with no error", err, j.em)
	}
}

// A panic during the synchronous start (snapshot builder or write) must end
// the stream with an error terminal before the crash re-surfaces: the run's
// own panic recovery is armed only after start returns.
func TestRunJSONOutStartPanicEmitsErrorTerminal(t *testing.T) {
	buf := &bytes.Buffer{}
	j := &jsonOut{em: tuistat.NewEmitter(buf, time.Hour), prog: nil} // nil prog: the builder panics
	func() {
		defer func() { _ = recover() }() // swallow the re-panic; the terminal is the point
		j.start()
	}()
	snaps := decodeJSONL(t, buf.String())
	if len(snaps) != 2 {
		t.Fatalf("want exactly the terminal + summary lines, got %d: %v", len(snaps), snaps)
	}
	if snaps[0]["event"] != "error" || snaps[0]["tool"] != "sfl" {
		t.Fatalf("terminal = %v/%v, want error/sfl", snaps[0]["event"], snaps[0]["tool"])
	}
	if e, _ := snaps[0]["error"].(string); !strings.Contains(e, "panic:") {
		t.Fatalf("terminal error = %q, want the panic message", e)
	}
	// Even a panicked start still ends with the summary line, minimal
	// (the panicking builder is not re-entered) but identifiable.
	if snaps[1]["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", snaps[1]["event"])
	}
	if _, ok := snaps[1]["summary"]; ok {
		t.Fatalf("start-panic summary must be envelope-only, got %v", snaps[1]["summary"])
	}
}

func TestRunJSONOutWritesNDJSONStream(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
		[]byte("URL: https://a.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(dir, "stats.jsonl")
	if err := run(runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"), Workers: 1, NoTUI: true,
		RunStamp: "20260922_010000",
		JSONOut:  jsonl, JSONEvery: 200 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	snaps := readJSONL(t, jsonl)
	if len(snaps) < 2 {
		t.Fatalf("want start + terminal, got %d lines", len(snaps))
	}
	first, last := snaps[0], snaps[len(snaps)-1]
	if first["event"] != "start" {
		t.Fatalf("first event = %v, want start", first["event"])
	}
	if first["tool"] != "sfl" {
		t.Fatalf("tool = %v, want sfl", first["tool"])
	}
	if last["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", last["event"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" {
		t.Fatalf("terminal event = %v, want done", terminal["event"])
	}
	if terminal["phase"] != "done" {
		t.Fatalf("terminal phase = %v, want done", terminal["phase"])
	}
	// The terminal snapshot carries the run's final counters.
	if lines, ok := terminal["lines"].(map[string]any); !ok || lines["unique"] != float64(1) {
		t.Fatalf("terminal lines block = %v, want unique=1", terminal["lines"])
	}
	// The summary line mirrors the human recap for the same fixture:
	// 1 unique credential from 1 file, no drops, output path present.
	sum, ok := last["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", last)
	}
	lines, ok := sum["lines"].(map[string]any)
	if !ok || lines["unique"] != float64(1) || lines["accepted"] != float64(1) {
		t.Fatalf("summary lines = %v, want accepted=1 unique=1", sum["lines"])
	}
	src, ok := sum["sources"].(map[string]any)
	if !ok || src["files"] != float64(1) {
		t.Fatalf("summary sources = %v, want files=1", sum["sources"])
	}
	if _, ok := sum["rejects"]; ok {
		t.Fatalf("clean run must omit the rejects breakdown: %v", sum)
	}
	out, ok := sum["output"].(map[string]any)
	if !ok {
		t.Fatalf("summary output block missing: %v", sum)
	}
	paths, _ := out["paths"].([]any)
	if len(paths) != 1 || !strings.Contains(paths[0].(string), "sfl") {
		t.Fatalf("summary output paths = %v, want the committed output", out["paths"])
	}
	// Elapsed time lives on the line's envelope only; the summary block must
	// not carry its own (the emitter stamps Snapshot.ElapsedMS).
	if _, ok := sum["elapsed_ms"]; ok {
		t.Fatalf("summary block must not duplicate elapsed_ms: %v", sum)
	}
	if ms, ok := last["elapsed_ms"].(float64); !ok || ms < 0 {
		t.Fatalf("summary line envelope elapsed_ms missing: %v", last)
	}
}

// Builder-level mapping test: stuffed counters on both the extraction stats
// and the -od ingest metrics must land in the summary rollup with the exact
// per-reason breakdown the human summary renders.
func TestJSONOutSummaryBlockStuffedCounters(t *testing.T) {
	j := &jsonOut{prog: sflog.NewProgress()}
	j.sum = summaryData{
		libDir: "/data/Library",
		stats: sflog.ExtractStats{
			FilesScanned: 4, ArchivesScanned: 2, Logs: 3,
			Credentials: 10, Emitted: 8, Duplicates: 2,
			SkippedFiles: 1, SkippedArchives: 1,
			HistoryChecked: 6, HistorySkipped: 1,
			EnvCopied: 5, EnvDeduped: 2, EnvDirsCopied: 1, EnvSkippedOverCap: 1,
		},
		haveStats: true,
	}
	met := &ulpengine.Metrics{}
	met.LinesUnique.Store(5)        // added to the library
	met.LinesSkippedByDest.Store(2) // already in library
	met.LinesRejected.Store(1)      // library refused
	j.sum.ingestMet = met

	b := j.summaryBlock()
	if b.Lines == nil || b.Lines.Accepted != 10 || b.Lines.Unique != 8 || b.Lines.Dupes != 2 {
		t.Fatalf("lines = %+v, want accepted=10 unique=8 dupes=2", b.Lines)
	}
	if b.Sources == nil || b.Sources.Files != 4 || b.Sources.Archives != 2 || b.Sources.LogsDone != 3 || b.Sources.Skipped != 2 {
		t.Fatalf("sources = %+v, want files=4 archives=2 logs=3 skipped=2", b.Sources)
	}
	if b.History == nil || b.History.Checked != 6 || b.History.Skipped != 1 {
		t.Fatalf("history = %+v, want checked=6 skipped=1", b.History)
	}
	if b.Env == nil || b.Env.Copied != 5 || b.Env.Deduped != 2 || b.Env.DirsCopied != 1 || b.Env.SkippedOverCap != 1 {
		t.Fatalf("env = %+v, want copied=5 deduped=2 dirs=1 overcap=1", b.Env)
	}
	if b.Rejects == nil ||
		b.Rejects.Dupes != 2 || b.Rejects.InLibrary != 2 || b.Rejects.LibraryRefused != 1 || b.Rejects.Total != 5 {
		t.Fatalf("rejects = %+v, want dupes=2 in_library=2 library_refused=1 total=5", b.Rejects)
	}
	if b.Library == nil || b.Library.Added != 5 {
		t.Fatalf("library = %+v, want added=5", b.Library)
	}
	if b.Output == nil || len(b.Output.Paths) != 1 || b.Output.Paths[0] != "/data/Library" {
		t.Fatalf("output = %+v, want the library dir", b.Output)
	}
}

// A run that fails after the stream opened must end in "error", never "done".
// Deterministic failure: -del cannot remove the source because its parent
// directory is read-only, which fails after extraction and output succeeded.
func TestRunJSONOutErrorTerminalOnDelFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions, so -del cannot be made to fail")
	}
	dir := t.TempDir()
	inputDir := filepath.Join(dir, "in")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "All Passwords.txt"),
		[]byte("URL: https://a.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(inputDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(inputDir, 0o755) }) // let TempDir cleanup remove it
	jsonl := filepath.Join(dir, "stats.jsonl")

	err := run(runConfig{
		Input: inputDir, OutputDir: filepath.Join(dir, "out"), Workers: 1, NoTUI: true,
		RunStamp: "20260922_012000", DeleteSources: true,
		JSONOut: jsonl, JSONEvery: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("del failure must fail the run")
	}
	snaps := readJSONL(t, jsonl)
	first, last := snaps[0], snaps[len(snaps)-1]
	if first["event"] != "start" {
		t.Fatalf("first event = %v, want start", first["event"])
	}
	if last["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", last["event"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "error" {
		t.Fatalf("terminal event = %v, want error (run failed: %v)", terminal["event"], err)
	}
	if terminal["error"] == "" {
		t.Fatal("terminal error event must carry the failure message")
	}
}

// Stdout mode: JSONOut "-" writes the same stream to os.Stdout. Swap the
// sink, run, then assert on the captured buffer.
func TestRunJSONOutStdoutTarget(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
		[]byte("URL: https://b.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pb := swapStdoutForTest(t)
	if err := run(runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"), Workers: 1, NoTUI: true,
		RunStamp: "20260922_011000",
		JSONOut:  "-", JSONEvery: 200 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	pb.closeWriter()
	snaps := decodeJSONL(t, pb.String())
	if snaps[0]["event"] != "start" {
		t.Fatalf("stdout stream first = %v, want start", snaps[0]["event"])
	}
	if snaps[len(snaps)-1]["event"] != "summary" || snaps[len(snaps)-2]["event"] != "done" {
		t.Fatalf("stdout stream endpoints wrong: terminal=%v last=%v",
			snaps[len(snaps)-2]["event"], snaps[len(snaps)-1]["event"])
	}
}

// swapStdoutForTest replaces os.Stdout with a pipe and returns a handle whose
// String() blocks until the pipe writer is closed (cleanup) and returns
// everything written. The original os.Stdout is restored on cleanup.
func swapStdoutForTest(t *testing.T) *pipeBuffer {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	pb := &pipeBuffer{r: r, w: w, done: make(chan struct{})}
	go func() {
		defer close(pb.done)
		var all []byte
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				pb.data = all
				return
			}
		}
	}()
	t.Cleanup(func() {
		os.Stdout = old
		w.Close()
		<-pb.done
	})
	return pb
}

// pipeBuffer collects a drained pipe's bytes.
type pipeBuffer struct {
	r    *os.File
	w    *os.File
	data []byte
	done chan struct{}
}

func (p *pipeBuffer) String() string {
	<-p.done
	return string(p.data)
}

// closeWriter ends the capture: the reader sees EOF, drains, and makes the
// bytes available to String().

func TestRunJSONOutCollisionPreflight(t *testing.T) {
	tests := []struct {
		name  string
		setup func(string) (runConfig, string)
	}{
		{"input", func(dir string) (runConfig, string) {
			input := filepath.Join(dir, "Passwords.txt")
			writeTestBytes(t, input)
			return runConfig{Input: input, OutputDir: filepath.Join(dir, "out"), RunStamp: "20260922_AAAAAA", JSONOut: input}, input
		}},
		{"input-tree", func(dir string) (runConfig, string) {
			input := filepath.Join(dir, "inputs")
			if err := os.MkdirAll(input, 0o755); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(input, "stats.jsonl")
			writeTestBytes(t, victim)
			return runConfig{Input: input, OutputDir: filepath.Join(dir, "out"), RunStamp: "20260922_AAAAAA", JSONOut: victim}, victim
		}},
		{"password-list", func(dir string) (runConfig, string) {
			input := filepath.Join(dir, "input.txt")
			writeTestBytes(t, input)
			pw := filepath.Join(dir, "passwords.txt")
			writeTestBytes(t, pw)
			return runConfig{Input: input, Password: pw, OutputDir: filepath.Join(dir, "out"), RunStamp: "20260922_AAAAAA", JSONOut: pw}, pw
		}},
		{"history", func(dir string) (runConfig, string) {
			input := filepath.Join(dir, "input.txt")
			writeTestBytes(t, input)
			h := filepath.Join(dir, "history.sqlite3")
			writeTestBytes(t, h)
			return runConfig{Input: input, History: true, HistoryPath: h, OutputDir: filepath.Join(dir, "out"), RunStamp: "20260922_AAAAAA", JSONOut: h}, h
		}},
		{"library", func(dir string) (runConfig, string) {
			input := filepath.Join(dir, "input.txt")
			writeTestBytes(t, input)
			lib := filepath.Join(dir, "library")
			victim := filepath.Join(lib, "stats.jsonl")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestBytes(t, victim)
			return runConfig{Input: input, LibraryDir: lib, OutputDir: filepath.Join(dir, "out"), RunStamp: "20260922_AAAAAA", JSONOut: victim}, victim
		}},
		{"classic-output", func(dir string) (runConfig, string) {
			input := filepath.Join(dir, "input.txt")
			writeTestBytes(t, input)
			out := filepath.Join(dir, "out")
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(out, "sfl_20260922_AAAAAA.txt")
			writeTestBytes(t, victim)
			return runConfig{Input: input, OutputDir: out, RunStamp: "20260922_AAAAAA", JSONOut: victim}, victim
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, victim := tc.setup(dir)
			outputDir := cfg.OutputDir
			_, outputStatErr := os.Stat(outputDir)
			outputDirExisted := outputStatErr == nil
			if outputStatErr != nil && !os.IsNotExist(outputStatErr) {
				t.Fatal(outputStatErr)
			}
			before, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			if err := run(cfg); err == nil || !strings.Contains(err.Error(), "invalid -json target") {
				t.Fatalf("run error = %v", err)
			}
			if !outputDirExisted {
				if _, err := os.Stat(outputDir); !os.IsNotExist(err) {
					t.Fatalf("collision validation created output dir %q: %v", outputDir, err)
				}
			}
			after, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("protected file changed")
			}
			if matches, _ := filepath.Glob(filepath.Join(dir, "sfl-od-*")); len(matches) != 0 {
				t.Fatalf("staging dirs created: %v", matches)
			}
			if _, err := os.Stat(filepath.Join(dir, "stats.jsonl")); !os.IsNotExist(err) {
				t.Fatalf("stats.jsonl created: %v", err)
			}
		})
	}
}

func writeTestBytes(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("sentinel bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertNoSflArtifacts fails when any classic output file, -od plaintext
// staging dir, or spill dir survived a rejected run under dir.
func assertNoSflArtifacts(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, "sfl-od-") || strings.HasPrefix(name, "sfl-spill-") {
			t.Errorf("staging/spill dir left behind: %s", path)
		} else if !d.IsDir() && strings.HasPrefix(name, "sfl_") {
			t.Errorf("classic output left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A non-positive -json-every is a usage error that must be rejected before the
// sink opens, so the rejected run leaves no output artifacts behind.
func TestRunJSONOutNonPositiveEveryBeforeSink(t *testing.T) {
	for _, tc := range []struct {
		name string
		od   bool
	}{
		{"classic", false},
		{"od", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "logs")
			if err := os.MkdirAll(input, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
				[]byte("URL: https://c.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg := runConfig{
				Input: input, Workers: 1, NoTUI: true, RunStamp: "20260922_030000",
				JSONOut: filepath.Join(dir, "stats.jsonl"), JSONEvery: 0,
			}
			if tc.od {
				cfg.LibraryDir = filepath.Join(dir, "lib")
			} else {
				cfg.OutputDir = filepath.Join(dir, "out")
			}
			err := run(cfg)
			if err == nil || !strings.Contains(err.Error(), "must be positive") {
				t.Fatalf("err = %v, want a -json-every positivity error", err)
			}
			assertNoSflArtifacts(t, dir)
			if _, statErr := os.Stat(cfg.JSONOut); !os.IsNotExist(statErr) {
				t.Fatalf("json stream created despite error: %v", statErr)
			}
		})
	}
}

// An unopenable -json target passes the preflight but fails after the sink
// opened; the armed sink discard must leave no empty classic output or -od
// staging behind.
func TestRunJSONOutUnopenableTargetBeforeSink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions, so the json target cannot be made unopenable")
	}
	for _, tc := range []struct {
		name string
		od   bool
	}{
		{"classic", false},
		{"od", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "logs")
			if err := os.MkdirAll(input, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
				[]byte("URL: https://d.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			ro := filepath.Join(dir, "ro")
			if err := os.MkdirAll(ro, 0o755); err != nil {
				t.Fatal(err)
			}
			jsonTarget := filepath.Join(ro, "stats.jsonl")
			if err := os.Chmod(ro, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(ro, 0o755) })
			cfg := runConfig{
				Input: input, Workers: 1, NoTUI: true, RunStamp: "20260922_040000",
				JSONOut: jsonTarget, JSONEvery: 200 * time.Millisecond,
			}
			if tc.od {
				cfg.LibraryDir = filepath.Join(dir, "lib")
			} else {
				cfg.OutputDir = filepath.Join(dir, "out")
			}
			err := run(cfg)
			if err == nil || !strings.Contains(err.Error(), "-json") {
				t.Fatalf("err = %v, want an open -json failure", err)
			}
			assertNoSflArtifacts(t, dir)
		})
	}
}

// assertOneInterruptedTerminal decodes the stream and requires: a start event
// first, exactly one interrupted terminal, and a summary line as the very
// last event, with no done/error event anywhere (a signal shutdown must
// never be classified as done/error).
func assertOneInterruptedTerminal(t *testing.T, snaps []map[string]any) {
	t.Helper()
	if len(snaps) < 2 {
		t.Fatalf("want start + terminal, got %d lines", len(snaps))
	}
	if snaps[0]["event"] != "start" {
		t.Fatalf("first event = %v, want start", snaps[0]["event"])
	}
	if last := snaps[len(snaps)-1]; last["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", last["event"])
	}
	if terminal := snaps[len(snaps)-2]; terminal["event"] != "interrupted" {
		t.Fatalf("terminal event = %v, want interrupted", terminal["event"])
	}
	interrupted := 0
	for _, s := range snaps {
		switch s["event"] {
		case "interrupted":
			interrupted++
		case "done", "error":
			t.Fatalf("done/error event after the signal: %v", s)
		}
	}
	if interrupted != 1 {
		t.Fatalf("want exactly one interrupted terminal, got %d", interrupted)
	}
}

// restoreSignalSeams saves and restores the P6-W8 signal test seams and swaps
// exitInterrupted for a recorder; the returned pointer reads 130 once the
// interrupted shutdown path ran. It also stubs termctl's force-exit hook:
// a run() whose graceful shutdown was stubbed still leaves its WatchInterrupt
// goroutine armed (signalForce stays true at ctx cancel), and without the
// stub that goroutine fires a real delayed os.Exit into whichever test runs
// more than interruptGrace later.
func restoreSignalSeams(t *testing.T) *int {
	t.Helper()
	oldForce, oldHook, oldExit, oldReg := signalForce, finalizeHook, exitInterrupted, reg
	t.Cleanup(func() { signalForce, finalizeHook, exitInterrupted, reg = oldForce, oldHook, oldExit, oldReg })
	// Defuse the delayed force-exit for the rest of the test binary, with no
	// restore: a run() whose graceful shutdown was stubbed still leaves its
	// WatchInterrupt goroutine armed (signalForce stays true when the run's
	// context cancels), and that goroutine fires a real os.Exit interruptGrace
	// later — after this test's cleanup has already restored the exit seams —
	// killing whatever test runs next. The force-exit output path itself is
	// covered by internal/termctl tests; nothing in this binary needs the
	// real hook while stubbed seams are in play.
	termctl.SetForceExitHook(func() {})
	// Isolate the registry for the same leaked watcher: interruptGrace after
	// the stubbed run's context cancels, its WatchInterrupt goroutine calls
	// ForceExit on whatever registry the run was given. On the shared
	// registry that force-exit lands in whichever test is running 5s later —
	// under full-suite parallel load the next in-process run() is often still
	// alive with its -json exit hook armed, and ForceExit closes that
	// stream with an interrupted terminal (the
	// TestRunJSONOutHistoryOnlyLiveProgress "interrupted/done" flake). The
	// leaked goroutine captures this test-private registry, so the stale
	// force-exit can never reach a later test's stream.
	reg = termctl.New(io.Discard, nil)
	exitCode := 0
	exitInterrupted = func() { exitCode = 130 }
	return &exitCode
}

// A signal that arrives while the sink is finalizing must win the terminal
// outcome: exactly one interrupted JSON event, exit 130, never done/error —
// and the output finalize had already committed stays on disk.
func TestRunJSONOutInterruptedDuringFinalize(t *testing.T) {
	exitCode := restoreSignalSeams(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
		[]byte("URL: https://sig.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(dir, "stats.jsonl")
	out := filepath.Join(dir, "out")
	forced := false
	finalizeHook = func() { forced = true }
	signalForce = func() bool { return forced }

	if err := run(runConfig{
		Input: input, OutputDir: out, Workers: 1, NoTUI: true,
		RunStamp: "20260922_060000",
		JSONOut:  jsonl, JSONEvery: 200 * time.Millisecond,
	}); err != nil {
		t.Fatalf("interrupted run returned err=%v, want nil (exit code carries the outcome)", err)
	}
	assertOneInterruptedTerminal(t, readJSONL(t, jsonl))
	if *exitCode != 130 {
		t.Fatalf("interrupted exit code = %d, want 130", *exitCode)
	}

	matches, err := filepath.Glob(filepath.Join(out, "sfl_*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("committed output missing after interrupt: matches=%v err=%v", matches, err)
	}
}

// A signal that arrives while history is being recorded must win the terminal
// outcome: exactly one interrupted JSON event, exit 130, never done/error.
func TestRunJSONOutInterruptedDuringHistory(t *testing.T) {
	exitCode := restoreSignalSeams(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
		[]byte("URL: https://sigh.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(dir, "stats.jsonl")
	out := filepath.Join(dir, "out")
	db := filepath.Join(dir, "history.sqlite3")
	forced := false
	oldSync := syncHistoryOutputs
	syncHistoryOutputs = func(paths []string) error {
		forced = true
		return oldSync(paths)
	}
	t.Cleanup(func() { syncHistoryOutputs = oldSync })
	signalForce = func() bool { return forced }

	if err := run(runConfig{
		Input: input, OutputDir: out, Workers: 1, NoTUI: true,
		RunStamp: "20260922_061000", History: true, HistoryPath: db,
		JSONOut: jsonl, JSONEvery: 200 * time.Millisecond,
	}); err != nil {
		t.Fatalf("interrupted run returned err=%v, want nil (exit code carries the outcome)", err)
	}
	assertOneInterruptedTerminal(t, readJSONL(t, jsonl))
	if *exitCode != 130 {
		t.Fatalf("interrupted exit code = %d, want 130", *exitCode)
	}

	matches, err := filepath.Glob(filepath.Join(out, "sfl_*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("committed output missing after interrupt: matches=%v err=%v", matches, err)
	}
}

// TestRunJSONOutPartialFailureCarriesExitCode: the terminal error event of a
// partial-failure run carries the exit code (internal/exitcode) in the
// additive `code` field, so stream consumers can branch without parsing the
// message.
func TestRunJSONOutPartialFailureCarriesExitCode(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	for _, sub := range []string{"good", "empty"} {
		if err := os.MkdirAll(filepath.Join(input, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(input, "good", "Passwords.txt"), []byte("URL: https://code.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "empty", "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jsonl := filepath.Join(dir, "stats.jsonl")
	err := run(runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"), Workers: 1, NoTUI: true,
		RunStamp: "20260924_010000", JSONOut: jsonl, JSONEvery: time.Millisecond,
	})
	var outcome *runOutcome
	if err == nil || !errors.As(err, &outcome) || outcome.code != exitcode.Partial {
		t.Fatalf("partial run err = %v, want runOutcome 3", err)
	}
	snaps := readJSONL(t, jsonl)
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "error" {
		t.Fatalf("terminal event = %v, want error", terminal["event"])
	}
	if terminal["code"] != float64(exitcode.Partial) {
		t.Fatalf("terminal code = %v, want %d", terminal["code"], exitcode.Partial)
	}
	// The summary line rides after the terminal, unchanged.
	if last := snaps[len(snaps)-1]; last["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", last["event"])
	}
	if code, ok := snaps[len(snaps)-1]["code"]; ok {
		t.Fatalf("summary event must not carry a code; got %v", code)
	}
}

func TestRunJSONOutCollisionDistinctPathAllowed(t *testing.T) {
	dir := t.TempDir()
	// The input must be a recognized sfl source (a credential-named file) so
	// the run discovers it and exits clean under the exit-code policy.
	inputDir := filepath.Join(dir, "input", "victim")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Parseable credentials so the run discovers the source and exits clean
	// under the exit-code policy.
	if err := os.WriteFile(filepath.Join(inputDir, "Passwords.txt"), []byte("https://collide.example:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input")
	jsonl := filepath.Join(dir, "stats.jsonl")
	err := run(runConfig{Input: input, OutputDir: filepath.Join(dir, "out"), RunStamp: "20260922_AAAAAA", JSONOut: jsonl, JSONEvery: time.Millisecond, NoTUI: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jsonl); err != nil {
		t.Fatalf("json output missing: %v", err)
	}
}

// writeRepeatedInput writes line repeated reps times under dir, streaming
// through a bounded buffer: a 1.7 GB single string plus the []byte copy
// os.WriteFile forces peaked one -race test process at ~12 GB, which
// systemd-oomd kills on 16 GB CI runners (exit 143). File bytes are
// identical to strings.Repeat(line, reps).
func writeRepeatedInput(t *testing.T, dir, name, line string, reps int64) string {
	t.Helper()
	const perBlock = 1024
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	block := strings.Repeat(line, perBlock)
	for i := reps / perBlock; i > 0; i-- {
		if _, err := w.WriteString(block); err != nil {
			f.Close()
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if rem := reps % perBlock; rem > 0 {
		if _, err := w.WriteString(strings.Repeat(line, int(rem))); err != nil {
			f.Close()
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		t.Fatalf("flush %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return path
}

// The checking-history phase streams live for sfl too: with a pre-seeded
// history database and a source big enough to outlast one update tick (the
// prehash runs ~3 GB/s per worker), an update lands with phase=
// checking-history and advancing bytes_done, and the all-hit run still ends
// done → summary with the history checked/skipped tallies.
func TestRunJSONOutHistoryOnlyLiveProgress(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	// The sampled fingerprint reads only head+tail, so the 1.7 GB source
	// no longer spans multiple 210ms ticks on its own; the test-only read
	// pause stretches each sample past the tick instead. 64 MiB is plenty.
	t.Setenv("SNOWFAST_TEST_FP_READ_PAUSE_MS", "150")
	line := "URL: https://a.example.com/login\nUSER: u\nPASS: p\n"
	reps := (int64(64) << 20) / int64(len(line))
	big := writeRepeatedInput(t, input, "All Passwords.txt", line, reps)

	db := filepath.Join(dir, "history.sqlite3")
	store, err := history.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	cand, err := history.FingerprintFile(context.Background(), big, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Record(context.Background(), []history.Identity{cand.ID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	size := cand.ID.Size

	jsonl := filepath.Join(dir, "stats.jsonl")
	if err := run(runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"), Workers: 1, NoTUI: true,
		RunStamp: "20260922_070000", History: true, HistoryPath: db,
		JSONOut: jsonl, JSONEvery: 210 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	snaps := readJSONL(t, jsonl)
	if snaps[0]["event"] != "start" || snaps[0]["tool"] != "sfl" {
		t.Fatalf("first line = %v/%v, want start/sfl", snaps[0]["event"], snaps[0]["tool"])
	}
	var sawLive bool
	prevDone := int64(-1)
	for _, s := range snaps {
		if s["event"] != "update" || s["phase"] != "checking-history" {
			continue
		}
		h, ok := s["history"].(map[string]any)
		if !ok || h["bytes_total"] != float64(size) {
			t.Fatalf("history update = %v, want bytes_total=%d", s, size)
		}
		doneAny, doneOk := h["bytes_done"].(float64)
		if !doneOk || doneAny == 0 {
			// Ticks fired before the first sampled read landed carry no
			// bytes yet (omitempty); they are updates, just not live ones.
			continue
		}
		// Live progress invariant: bytes_done never rewinds across observed
		// ticks, however few or many the scheduler lets through under load.
		done := int64(h["bytes_done"].(float64))
		if prevDone >= 0 && done < prevDone {
			t.Fatalf("checking-history bytes_done rewound: %d after %d", done, prevDone)
		}
		prevDone = done
		sawLive = true
	}
	if !sawLive {
		t.Fatalf("no update with live checking-history bytes: %d lines", len(snaps))
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
	sum, ok := snaps[len(snaps)-1]["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", snaps[len(snaps)-1])
	}
	h, ok := sum["history"].(map[string]any)
	if !ok || h["checked"] != float64(1) || h["skipped"] != float64(1) {
		t.Fatalf("summary history = %v, want checked=1 skipped=1", sum["history"])
	}
}
func (p *pipeBuffer) closeWriter() { p.w.Close() }

// A force-exit (second Ctrl-C or cleanup timeout) fires on the signal/watcher
// goroutine, where the deferred stop never gets to run. The registry's exit
// hook — armed by jout.start() — must close the stream there: interrupted
// terminal with the force-exit reason in the error field, then summary as
// the very last line, all before the os.Exit seam fires. A stale call after
// the hook's own stop() must not fire the hook again (it disarms itself).
func TestRunJSONOutExitHookClosesStreamOnForceExit(t *testing.T) {
	// No watcher goroutine is armed here, so a no-op seam stub is safe; like
	// restoreSignalSeams it is deliberately NOT restored to the production
	// os.Exit in cleanup — a stale armed watcher must never regain a real
	// delayed process kill. The recording seam below is swapped back to this
	// no-op before the test ends.
	termctl.SetForceExitHook(func() {})
	reg := termctl.New(io.Discard, nil)
	jsonl := filepath.Join(t.TempDir(), "stats.jsonl")
	j, err := newJSONOut(runConfig{JSONOut: jsonl, JSONEvery: 50 * time.Millisecond}, sflog.NewProgress(), reg)
	if err != nil {
		t.Fatal(err)
	}
	j.start()

	var order []string
	// Wrap the hook start() armed so the ordering against the os.Exit seam
	// is observable; the wrapped body is exactly what the hook runs.
	reg.SetExitHook(func(reason string) {
		order = append(order, "hook:"+reason)
		j.stop(tuistat.EventInterrupted, reason)
	})
	termctl.SetForceExitHook(func() { order = append(order, "seam") })
	defer termctl.SetForceExitHook(func() {})

	reg.ForceExit("force-exit (signal received twice).")
	want := []string{"hook:force-exit (signal received twice).", "seam"}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("ForceExit order = %v, want %v", order, want)
	}

	snaps := readJSONL(t, jsonl)
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "interrupted" {
		t.Fatalf("terminal = %v, want interrupted", terminal["event"])
	}
	if e, _ := terminal["error"].(string); e != "force-exit (signal received twice)." {
		t.Fatalf("terminal error = %q, want the force-exit reason", e)
	}
	if snaps[len(snaps)-1]["event"] != "summary" {
		t.Fatalf("last line = %v, want summary", snaps[len(snaps)-1]["event"])
	}

	// The hook's stop() disarmed itself: a second ForceExit (the seam is
	// still stubbed, the process not yet gone) must not touch the stream
	// again — no second hook entry, no double terminal.
	order = nil
	reg.ForceExit("force-exit (signal received twice).")
	if len(order) != 1 || order[0] != "seam" {
		t.Fatalf("post-stop ForceExit order = %v, want [seam] (hook must be disarmed)", order)
	}
	if snaps2 := readJSONL(t, jsonl); len(snaps2) != len(snaps) {
		t.Fatalf("post-stop ForceExit appended lines: %d -> %d", len(snaps), len(snaps2))
	}
}
