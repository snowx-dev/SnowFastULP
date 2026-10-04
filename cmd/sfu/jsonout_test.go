package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// decodeSfuJSONL decodes every non-empty line of an NDJSON string.
func decodeSfuJSONL(t *testing.T, data string) []map[string]any {
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

func TestSFUJSONOutStream(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "stats.jsonl")

	m := &ulpengine.Metrics{TotalInputBytes: 1000}
	m.Phase.Store(ulpengine.PhaseShard)
	r := &ulpengine.Resolved{TotalInputs: 1000, Workers: 2, DedupWorkers: 1}

	j, err := newJSONOut(jsonl, 100*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	j.setEngine(m, r)
	j.start()
	// Advance the engine while the stream runs so an update snapshot must
	// appear with fresh counters.
	m.BytesRead.Store(500)
	m.LinesAccepted.Store(40)
	if !waitForSfu(2*time.Second, func() bool {
		data, err := os.ReadFile(jsonl)
		if err != nil {
			return false
		}
		// Count complete lines only: a read can observe a torn trailing
		// record while the emitter is mid-write. Strict decoding waits for
		// the stable post-stop file below.
		return strings.Count(string(data), "\n") >= 2
	}) {
		t.Fatal("no update snapshot within 2s")
	}
	m.Phase.Store(ulpengine.PhaseDone)
	m.LinesUnique.Store(40)
	j.stop("done", "")

	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" || snaps[0]["tool"] != "sfu" {
		t.Fatalf("first line = %v/%v, want start/sfu", snaps[0]["event"], snaps[0]["tool"])
	}
	last := snaps[len(snaps)-1]
	if last["event"] != "summary" {
		t.Fatalf("last line = %v, want summary", last["event"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal line = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
	if lines, ok := terminal["lines"].(map[string]any); !ok || lines["unique"] != float64(40) {
		t.Fatalf("terminal lines = %v, want unique=40", terminal["lines"])
	}
	// The summary mirrors the human DONE recap: 40 accepted, 40 unique, no
	// drops (dupes derive to 0 and are omitted), input bytes, elapsed.
	sum, ok := last["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", last)
	}
	lines, ok := sum["lines"].(map[string]any)
	if !ok || lines["accepted"] != float64(40) || lines["unique"] != float64(40) {
		t.Fatalf("summary lines = %v, want accepted=40 unique=40", sum["lines"])
	}
	if _, ok := sum["rejects"]; ok {
		t.Fatalf("drop-free run must omit the rejects breakdown: %v", sum)
	}
	by, ok := sum["bytes"].(map[string]any)
	if !ok || by["read"] != float64(500) {
		t.Fatalf("summary bytes = %v, want read=500", sum["bytes"])
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

// Builder-level mapping test: stuffed engine counters land in the summary
// rollup with the per-reason breakdown the human DONE recap renders.
func TestSFUJSONOutSummaryBlockStuffedCounters(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesRead.Store(100)
	m.LinesAccepted.Store(85)
	m.LinesRejected.Store(5) // 1 too long + 1 malformed + 1 password>64 + 2 unrepresentable
	m.LinesTooLong.Store(1)
	m.LinesMalformed.Store(1)
	m.LinesUnrepresentable.Store(2)
	m.LinesPasswordTooLong.Store(1)
	m.LinesUnique.Store(40)
	m.LinesSkippedByDest.Store(9)
	m.BytesRead.Store(1 << 20)
	m.BytesWritten.Store(1 << 19)
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 20, InputFileCount: 3, Workers: 4, DedupWorkers: 2,
		HistoryChecked: 7, HistorySkipped: 2,
		OutputPaths: []string{"/out/sfu_output.txt"},
	}

	j := &jsonOut{}
	j.setEngine(m, r)
	b := j.summaryBlock()
	if b.Lines == nil ||
		b.Lines.Read != 100 || b.Lines.Accepted != 85 || b.Lines.Rejected != 5 ||
		b.Lines.Unique != 40 || b.Lines.InLibrary != 9 || b.Lines.Unrepresentable != 2 {
		t.Fatalf("lines = %+v, want read=100 accepted=85 rejected=5 unique=40 in_library=9 unrepresentable=2", b.Lines)
	}
	// genuine dupes = accepted - unique - in_library = 85-40-9 = 36
	if b.Rejects == nil ||
		b.Rejects.Total != 5+36+9 || b.Rejects.Dupes != 36 ||
		b.Rejects.TooLong != 1 || b.Rejects.Malformed != 1 ||
		b.Rejects.PasswordTooLong != 1 || b.Rejects.Unrepresentable != 2 {
		t.Fatalf("rejects = %+v, want total=50 dupes=36 too_long=1 malformed=1 password_too_long=1 unrepresentable=2", b.Rejects)
	}
	if b.Bytes == nil || b.Bytes.Read != 1<<20 || b.Bytes.Written != 1<<19 {
		t.Fatalf("bytes = %+v, want read+written", b.Bytes)
	}
	if b.Sources == nil || b.Sources.Files != 3 {
		t.Fatalf("sources = %+v, want files=3", b.Sources)
	}
	if b.History == nil || b.History.Checked != 7 || b.History.Skipped != 2 {
		t.Fatalf("history = %+v, want checked=7 skipped=2", b.History)
	}
	if b.Output == nil || len(b.Output.Paths) != 1 {
		t.Fatalf("output = %+v, want the output path", b.Output)
	}
}

// A panic during the synchronous start (snapshot builder or write) must end
// the stream with an error terminal before the crash re-surfaces: main's
// recover hook reads joutRef, which is assigned only after start returns.
// The production provider is nil-safe (the stream opens before the engine
// exists), so the test injects a panicking builder through the seam start()
// consults.
func TestSFUJSONOutStartPanicEmitsErrorTerminal(t *testing.T) {
	buf := &bytes.Buffer{}
	j := &jsonOut{em: tuistat.NewEmitter(buf, time.Hour)}
	j.providerForTest = func(time.Time) tuistat.Snapshot { panic("boom") }
	func() {
		defer func() { _ = recover() }() // swallow the re-panic; the terminal is the point
		j.start()
	}()
	snaps := decodeSfuJSONL(t, buf.String())
	if len(snaps) != 2 {
		t.Fatalf("want exactly the terminal + summary lines, got %d: %v", len(snaps), snaps)
	}
	if snaps[0]["event"] != "error" || snaps[0]["tool"] != "sfu" {
		t.Fatalf("terminal = %v/%v, want error/sfu", snaps[0]["event"], snaps[0]["tool"])
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

// A non-positive interval is a usage error when the stream is on, and inert
// while the stream is off.
func TestSFUJSONOutRejectsNonPositiveEvery(t *testing.T) {
	for _, every := range []time.Duration{0, -time.Second} {
		j, err := newJSONOut(filepath.Join(t.TempDir(), "stats.jsonl"), every, nil)
		if err == nil || !strings.Contains(err.Error(), "must be positive") {
			t.Fatalf("every=%v: err=%v, want a positivity error", every, err)
		}
		if j != nil {
			t.Fatalf("every=%v: stream created despite error", every)
		}
	}
}

func TestSFUJSONOutOffAndBadPath(t *testing.T) {
	j, err := newJSONOut("", time.Second, nil)
	if err != nil || j.em != nil {
		t.Fatalf("empty target must be a no-op emitter, got %v/%v", j, err)
	}
	if _, err := newJSONOut(filepath.Join(t.TempDir(), "no-such-dir", "s.jsonl"), time.Second, nil); err == nil {
		t.Fatal("unopenable file must error")
	}
}

// A run without -history hands the engine to the stream before it opens:
// start builds the first snapshot synchronously, so without setEngine first
// the start line would be mislabeled checking-history. Pin the history-less
// start line: parsing phase, bytes/workers blocks, no history block.
func TestSFUJSONOutHistorylessStartLineIsParsing(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "stats.jsonl")

	m := &ulpengine.Metrics{TotalInputBytes: 5000}
	m.Phase.Store(ulpengine.PhaseInit)
	r := &ulpengine.Resolved{TotalInputs: 5000, Workers: 4, DedupWorkers: 1}

	j, err := newJSONOut(jsonl, time.Hour, nil) // clamps to the 200ms floor; stop lands before any tick
	if err != nil {
		t.Fatal(err)
	}
	j.setEngine(m, r) // the ordering main uses when no history phase opened the stream
	j.start()
	j.stop("done", "")

	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" || snaps[0]["phase"] != "parsing" {
		t.Fatalf("first line = %v/%v, want start/parsing", snaps[0]["event"], snaps[0]["phase"])
	}
	if h, ok := snaps[0]["history"]; ok {
		t.Fatalf("history-less start line carries a history block: %v", h)
	}
	b, ok := snaps[0]["bytes"].(map[string]any)
	if !ok || b["total"] != float64(5000) {
		t.Fatalf("start bytes = %v, want total=5000", snaps[0]["bytes"])
	}
	w, ok := snaps[0]["workers"].(map[string]any)
	if !ok || w["total"] != float64(4) {
		t.Fatalf("start workers = %v, want total=4", snaps[0]["workers"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
}

// The checking-history phase is streamed before the engine exists: stuffed
// prehash counters surface on update ticks as history.bytes_done/total with
// phase=checking-history, and once setEngine hands over the stream switches
// to the engine phases with the history tallies riding the recap blocks.
func TestSFUJSONOutHistoryPhaseLiveUpdates(t *testing.T) {
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "stats.jsonl")

	hc := &historyCounters{}
	j, err := newJSONOut(jsonl, 50*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	j.hc = hc
	j.start() // engine unresolved: the stream opens in checking-history

	hc.record(0, 2, 0, 400)
	hc.record(1, 2, 700, 1500)
	if !waitForSfu(2*time.Second, func() bool {
		data, err := os.ReadFile(jsonl)
		if err != nil {
			return false
		}
		// Complete lines only: a read can observe a torn trailing record.
		return strings.Count(string(data), "\n") >= 2
	}) {
		t.Fatal("no update snapshot within 2s")
	}

	// Hand over to the engine mid-run, like main does after Resolve.
	m := &ulpengine.Metrics{TotalInputBytes: 1500}
	m.Phase.Store(ulpengine.PhaseShard)
	r := &ulpengine.Resolved{TotalInputs: 1500, Workers: 2, DedupWorkers: 1, HistoryChecked: 2, HistorySkipped: 1}
	hc.setSkipped(1)
	hc.record(2, 2, 1500, 1500)
	j.setEngine(m, r)
	// The emitter clamps to a 200ms floor, so the next tick lands ~200ms
	// after start; wait for an engine-phase update to prove the handover.
	if !waitForSfu(2*time.Second, func() bool {
		data, err := os.ReadFile(jsonl)
		if err != nil {
			return false
		}
		return strings.Contains(string(data), `"phase":"parsing"`)
	}) {
		t.Fatal("no engine-phase update within 2s after setEngine")
	}
	m.Phase.Store(ulpengine.PhaseDone)
	j.stop("done", "")

	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" || snaps[0]["phase"] != "checking-history" {
		t.Fatalf("first line = %v/%v, want start/checking-history", snaps[0]["event"], snaps[0]["phase"])
	}
	var sawLive bool
	var sawEngine bool
	for _, s := range snaps {
		if s["event"] != "update" {
			continue
		}
		if s["phase"] == "checking-history" {
			h, ok := s["history"].(map[string]any)
			if !ok || h["bytes_done"] == float64(0) || h["bytes_total"] != float64(1500) {
				t.Fatalf("history update = %v, want live bytes with total=1500", s)
			}
			if h["checked"] != float64(1) {
				t.Fatalf("history update = %v, want checked=1", s)
			}
			sawLive = true
		}
		if s["phase"] == "parsing" || s["phase"] == "done" {
			sawEngine = true
		}
	}
	if !sawLive {
		t.Fatalf("no update with phase=checking-history: %v", snaps)
	}
	if !sawEngine {
		t.Fatalf("no engine-phase update after handover: %v", snaps)
	}
	last := snaps[len(snaps)-1]
	if last["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", last["event"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
	sum, ok := last["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", last)
	}
	h, ok := sum["history"].(map[string]any)
	if !ok || h["checked"] != float64(2) || h["skipped"] != float64(1) {
		t.Fatalf("summary history = %v, want checked=2 skipped=1", sum["history"])
	}
}

// A history-only run (every source already completed) closes the stream on
// the run goroutine: terminal done in the done phase, then a summary whose
// history block carries checked/skipped — and nothing else, since no engine
// ever ran.
func TestSFUJSONOutHistoryOnlyDoneSummary(t *testing.T) {
	buf := &bytes.Buffer{}
	hc := &historyCounters{}
	j := &jsonOut{em: tuistat.NewEmitter(buf, time.Hour)}
	j.hc = hc
	j.start()
	hc.record(3, 3, 4096, 4096)
	hc.setSkipped(3)
	j.stop("done", "")

	snaps := decodeSfuJSONL(t, buf.String())
	if snaps[0]["event"] != "start" || snaps[0]["phase"] != "checking-history" {
		t.Fatalf("first line = %v/%v, want start/checking-history", snaps[0]["event"], snaps[0]["phase"])
	}
	if h, ok := snaps[0]["history"].(map[string]any); !ok || h["enabled"] != true {
		t.Fatalf("start history = %v, want enabled", snaps[0]["history"])
	}
	if len(snaps) != 3 {
		t.Fatalf("want exactly start, terminal, summary, got %d: %v", len(snaps), snaps)
	}
	terminal := snaps[1]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
	sum, ok := snaps[2]["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", snaps[2])
	}
	h, ok := sum["history"].(map[string]any)
	if !ok || h["checked"] != float64(3) || h["skipped"] != float64(3) {
		t.Fatalf("summary history = %v, want checked=3 skipped=3", sum["history"])
	}
	if _, ok := sum["lines"]; ok {
		t.Fatalf("history-only summary must not carry engine lines: %v", sum)
	}
}

// An interrupt during the history check keeps the checking-history phase on
// the terminal: the run never reached the engine phases.
func TestSFUJSONOutHistoryPhaseInterruptedTerminal(t *testing.T) {
	buf := &bytes.Buffer{}
	hc := &historyCounters{}
	j := &jsonOut{em: tuistat.NewEmitter(buf, time.Hour)}
	j.hc = hc
	j.start()
	hc.record(1, 4, 100, 400)
	j.stop("interrupted", "")

	snaps := decodeSfuJSONL(t, buf.String())
	if snaps[0]["event"] != "start" {
		t.Fatalf("first line = %v, want start", snaps[0]["event"])
	}
	terminal := snaps[1]
	if terminal["event"] != "interrupted" || terminal["phase"] != "checking-history" {
		t.Fatalf("terminal = %v/%v, want interrupted/checking-history", terminal["event"], terminal["phase"])
	}
	if h, ok := terminal["history"].(map[string]any); ok && h["bytes_done"] != float64(100) {
		t.Fatalf("terminal history = %v, want bytes_done=100", terminal["history"])
	}
	if snaps[2]["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", snaps[1]["event"])
	}
}

// --- helpers ---

func waitForSfu(deadline time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A force-exit (second Ctrl-C or cleanup timeout) fires on the signal/watcher
// goroutine, where the deferred stop never gets to run. The registry's exit
// hook — armed by jout.start() on both the history and history-less open
// paths — must close the stream there: interrupted terminal with the
// force-exit reason in the error field, then summary as the very last line,
// all before the os.Exit seam fires. A stale call after the hook's own
// stop() must not fire the hook again (it disarms itself).
func TestSFUJSONOutExitHookClosesStreamOnForceExit(t *testing.T) {
	// No watcher goroutine is armed here, so a no-op seam stub is safe; it is
	// deliberately NOT restored to the production os.Exit in cleanup — a
	// stale armed watcher must never regain a real delayed process kill. The
	// recording seam below is swapped back to this no-op before the end.
	termctl.SetForceExitHook(func() {})
	reg := termctl.New(io.Discard, nil)
	jsonl := filepath.Join(t.TempDir(), "stats.jsonl")
	j, err := newJSONOut(jsonl, 50*time.Millisecond, reg)
	if err != nil {
		t.Fatal(err)
	}
	m := &ulpengine.Metrics{TotalInputBytes: 1000}
	m.Phase.Store(ulpengine.PhaseShard)
	r := &ulpengine.Resolved{TotalInputs: 1000, Workers: 2, DedupWorkers: 1}
	j.setEngine(m, r)
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

	reg.ForceExit("force-exit: interrupted (cleanup timed out)")
	want := []string{"hook:force-exit: interrupted (cleanup timed out)", "seam"}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("ForceExit order = %v, want %v", order, want)
	}

	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	if snaps[0]["event"] != "start" {
		t.Fatalf("first line = %v, want start", snaps[0]["event"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "interrupted" {
		t.Fatalf("terminal = %v, want interrupted", terminal["event"])
	}
	if e, _ := terminal["error"].(string); e != "force-exit: interrupted (cleanup timed out)" {
		t.Fatalf("terminal error = %q, want the force-exit reason", e)
	}
	if snaps[len(snaps)-1]["event"] != "summary" {
		t.Fatalf("last line = %v, want summary", snaps[len(snaps)-1]["event"])
	}

	// The hook's stop() disarmed itself: a second ForceExit (the seam is
	// still stubbed, the process not yet gone) must not touch the stream
	// again — no second hook entry, no double terminal.
	order = nil
	reg.ForceExit("force-exit: interrupted (cleanup timed out)")
	if len(order) != 1 || order[0] != "seam" {
		t.Fatalf("post-stop ForceExit order = %v, want [seam] (hook must be disarmed)", order)
	}
	if snaps2 := decodeSfuJSONL(t, mustRead(t, jsonl)); len(snaps2) != len(snaps) {
		t.Fatalf("post-stop ForceExit appended lines: %d -> %d", len(snaps), len(snaps2))
	}
}
