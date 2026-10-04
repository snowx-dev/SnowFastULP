//go:build linux

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The interrupt contract under test: SIGINT once → the engine keeps draining
// (its read/shard pipeline does not cancel mid-read) → if that drain outlasts
// termctl's 5s interruptGrace the cleanup force-exits (exit 130) and the JSON
// stream ends interrupted→summary. Everything below exists to make "the drain
// outlasts the grace" provable instead of assumed.

// signalWindowFloor is the drain margin the signal must be preceded by: the
// 5s grace plus ~2s slack for tick staleness (≤210ms), byte-rate wobble, and
// the poll interval between a qualifying tick and the delivered signal.
const signalWindowFloor = 7 * time.Second

// forceExitReps sizes the fixture so even a fast runner still has
// signalWindowFloor of drain left at the first 210ms update tick. The failed
// CI run (run 35839586084) drained the old 10M-line fixture end-to-end in ~3s
// (~150MB/s) — the post-signal drain finished inside the 5s grace and the run
// completed normally with exit 0 before the cleanup-timeout path could fire.
// 70M lines (~3.1GB) keeps ≥7s of remaining drain at the first tick up to a
// ~400MB/s machine, ~2.5× the observed CI rate; a faster machine fails loudly
// in waitForSignalWindow instead of flaking.
const forceExitReps = 70_000_000

var (
	bigParseInputOnce sync.Once
	bigParseInputPath string
	bigParseInputErr  error
)

// bigParseInput writes the shared single-chunk input whose parse outlasts the
// 5s interrupt grace with the margin forceExitReps buys. Both force-exit e2e
// tests consume the same read-only file, so it is built once into its own
// temp dir (the package TestMain TMPDIR sandbox cleans it up); per-test
// fixture writes would double the ~3GB cost for no isolation benefit — the
// sfu child never mutates its inputs.
func bigParseInput(t *testing.T) string {
	t.Helper()
	bigParseInputOnce.Do(func() {
		dir, err := os.MkdirTemp("", "snowfast-sfu-biginput-")
		if err != nil {
			bigParseInputErr = err
			return
		}
		bigParseInputPath, bigParseInputErr = writeRepeatedInputErr(dir, "input.txt",
			"https://site.example.com:user:password123456\n", forceExitReps)
	})
	if bigParseInputErr != nil {
		t.Fatal(bigParseInputErr)
	}
	return bigParseInputPath
}

// updateTick is the subset of a -json update line the signal window
// needs. The bytes block carries the measured read rate the test converts
// into seconds of remaining drain.
type updateTick struct {
	Event    string  `json:"event"`
	Phase    string  `json:"phase"`
	Fraction float64 `json:"fraction"`
	Bytes    *struct {
		Read  int64   `json:"read"`
		Total int64   `json:"total"`
		BPS   float64 `json:"bps"`
	} `json:"bytes"`
}

// waitForSignalWindow polls the jsonl until the live parse provably has more
// than signalWindowFloor of drain left: the unread bytes divided by the
// highest byte-per-second rate observed so far must exceed the floor. The max
// over observed ticks keeps a warm-up underestimate from faking a longer
// window than the machine can actually deliver after the signal. Waiting for
// a bare first "update" line was the racy precondition: on fast CI hardware
// the whole run completed (exit 0) between that tick and the delivered
// SIGINT. A machine so fast that no tick ever qualifies fails loudly here
// naming the measured rate — a legible sizing failure, never a flake.
func waitForSignalWindow(t *testing.T, jsonl string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var maxBPS float64
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(jsonl)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		remaining := -1.0
		for _, line := range strings.Split(string(data), "\n") {
			var tick updateTick
			if json.Unmarshal([]byte(line), &tick) != nil {
				continue
			}
			if tick.Event != "update" || tick.Phase != "parsing" || tick.Bytes == nil {
				continue
			}
			b := tick.Bytes
			if b.BPS > maxBPS {
				maxBPS = b.BPS
			}
			if maxBPS > 0 {
				remaining = float64(b.Total-b.Read) / maxBPS
			}
			if remaining >= signalWindowFloor.Seconds() {
				return
			}
			// Past half the fixture with no qualifying tick means the host
			// out-parses the sizing margin; later ticks only shrink the
			// remaining work further.
			if tick.Fraction >= 0.5 {
				t.Fatalf("fixture too small for this machine: at fraction %.2f (%d/%d bytes) the best observed rate %.0f B/s leaves %.1fs of drain, below the %.0fs signal window",
					tick.Fraction, b.Read, b.Total, maxBPS, remaining, signalWindowFloor.Seconds())
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no update tick with ≥%v of remaining drain within %v (child never went live?)", signalWindowFloor, timeout)
}

// waitForUpdateGrowth waits until the jsonl holds more update lines than
// baseline, bounded by timeout — proof the child is still alive and draining
// after the first SIGINT, so a second signal lands mid-drain rather than on
// an already-finished run.
func waitForUpdateGrowth(t *testing.T, jsonl string, baseline int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(jsonl)
		if err == nil && strings.Count(string(data), `"event":"update"`) > baseline {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("update count never grew past %d within %v — the child was no longer draining", baseline, timeout)
}

// countUpdateLines snapshots how many update lines the jsonl currently holds.
func countUpdateLines(t *testing.T, jsonl string) int {
	t.Helper()
	data, err := os.ReadFile(jsonl)
	if err != nil {
		t.Fatalf("read %s: %v", jsonl, err)
	}
	return strings.Count(string(data), `"event":"update"`)
}

// waitE2EExit waits for the child to exit with the given code, killing it on
// timeout.
func waitE2EExit(t *testing.T, cmd *exec.Cmd, output *strings.Builder, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("wait: %v\noutput:\n%s", err, output)
		return -1
	case <-time.After(timeout):
		cmd.Process.Kill()
		t.Fatalf("run did not exit within %v\noutput:\n%s", timeout, output)
		return -1
	}
}

// A double SIGINT during a long parse must force-exit (exit 130) with the
// JSON stream closed by the registry's exit hook: the last two lines are the
// interrupted terminal (force-exit reason in the error field) then summary.
func TestSFUE2E_ForceExitSecondSignalClosesStream(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := bigParseInput(t)
	jsonl := filepath.Join(dir, "stats.jsonl")

	cmd := sfuE2ECommand(t, bin, dir, input, "-o", filepath.Join(dir, "out")+string(os.PathSeparator),
		"-json="+jsonl, "-json-every=210ms")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	}()

	// Wait until the live parse provably has ≥7s of drain left, then Ctrl-C
	// twice: the second signal lands mid-drain (a fresh update tick proves the
	// child is still running) and force-exits instead of waiting out the grace.
	waitForSignalWindow(t, jsonl, 30*time.Second)
	baseline := countUpdateLines(t, jsonl)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("first SIGINT: %v", err)
	}
	waitForUpdateGrowth(t, jsonl, baseline, 10*time.Second)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("second SIGINT: %v", err)
	}

	code := waitE2EExit(t, cmd, &stderr, 15*time.Second)
	if code != 130 {
		t.Fatalf("exit = %d, want 130\nstderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "force-exit (signal received twice).") {
		t.Fatalf("force-exit reason missing from stderr:\n%s", stderr.String())
	}
	assertForceExitStreamTail(t, jsonl, "force-exit (signal received twice).")
}

// A single SIGINT whose cleanup overruns the 5s grace must force-exit from
// the WatchInterrupt path and close the JSON stream the same way.
func TestSFUE2E_ForceExitCleanupTimeoutClosesStream(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := bigParseInput(t)
	jsonl := filepath.Join(dir, "stats.jsonl")

	cmd := sfuE2ECommand(t, bin, dir, input, "-o", filepath.Join(dir, "out")+string(os.PathSeparator),
		"-json="+jsonl, "-json-every=210ms")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	}()

	// A single SIGINT once the drain provably outlasts the 5s grace; the
	// cleanup must then overrun the grace and force-exit from the
	// WatchInterrupt path.
	waitForSignalWindow(t, jsonl, 30*time.Second)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}

	code := waitE2EExit(t, cmd, &stderr, 25*time.Second)
	if code != 130 {
		t.Fatalf("exit = %d, want 130\nstderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "force-exit: interrupted (cleanup timed out)") {
		t.Fatalf("cleanup-timeout reason missing from stderr:\n%s", stderr.String())
	}
	assertForceExitStreamTail(t, jsonl, "force-exit: interrupted (cleanup timed out)")
}

// assertForceExitStreamTail checks the stream contract on a force-exit: the
// last two NDJSON lines are the interrupted terminal carrying the force-exit
// reason, then the summary.
func assertForceExitStreamTail(t *testing.T, jsonl, reason string) {
	t.Helper()
	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if len(snaps) < 3 {
		t.Fatalf("stream too short: %d lines", len(snaps))
	}
	if snaps[0]["event"] != "start" {
		t.Fatalf("first line = %v, want start", snaps[0]["event"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "interrupted" {
		t.Fatalf("terminal = %v, want interrupted (full tail: %v)", terminal["event"], snaps[max(0, len(snaps)-3):])
	}
	if e, _ := terminal["error"].(string); e != reason {
		t.Fatalf("terminal error = %q, want %q", e, reason)
	}
	last := snaps[len(snaps)-1]
	if last["event"] != "summary" {
		t.Fatalf("last line = %v, want summary", last["event"])
	}
}
