package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
)

// decodeSFSJSONL decodes every non-empty line of an NDJSON string.
func decodeSFSJSONL(t *testing.T, data string) []map[string]any {
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

func waitForSFS(deadline time.Duration, cond func() bool) bool {
	deadlineAt := time.Now().Add(deadline)
	for time.Now().Before(deadlineAt) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func mustReadSFS(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSFSJSONOutStream(t *testing.T) {
	dir := t.TempDir()
	jsonl := dir + "/stats.jsonl"

	m := &search.Metrics{}
	m.ArchivesTotal.Store(3)
	m.Phase.Store(search.PhaseIndex)
	m.IndexBytesTotal.Store(1000)
	m.IndexBytesDone.Store(250)

	j, err := newJSONOut(jsonl, 100*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	j.setMetrics(m)
	j.start()
	// Advance the counters while the stream runs so an update snapshot must
	// appear with fresh numbers, then move to the search phase.
	if !waitForSFS(2*time.Second, func() bool {
		return strings.Count(mustReadSFS(t, jsonl), "\n") >= 2
	}) {
		t.Fatal("no update snapshot within 2s")
	}
	m.IndexBytesDone.Store(1000)
	m.Phase.Store(search.PhaseSearch)
	m.BytesScannedTotal.Store(4000)
	m.BytesScanned.Store(1200)
	m.ChunksTotal.Store(9)
	m.ChunksDone.Store(3)
	m.Hits.Store(7)
	m.ArchivesIndexed.Store(3)
	if !waitForSFS(2*time.Second, func() bool {
		return strings.Contains(mustReadSFS(t, jsonl), `"phase":"searching"`)
	}) {
		t.Fatal("no searching-phase update within 2s")
	}
	m.Phase.Store(search.PhaseDone)
	m.ChunksDone.Store(9)
	m.BytesScanned.Store(4000)
	m.ArchivesDone.Store(3)
	j.stop(tuistat.EventDone, "")

	snaps := decodeSFSJSONL(t, mustReadSFS(t, jsonl))
	if snaps[0]["event"] != "start" || snaps[0]["tool"] != "sfs" {
		t.Fatalf("first line = %v/%v, want start/sfs", snaps[0]["event"], snaps[0]["tool"])
	}
	last := snaps[len(snaps)-1]
	if last["event"] != "summary" {
		t.Fatalf("last line = %v, want summary", last["event"])
	}
	terminal := snaps[len(snaps)-2]
	if terminal["event"] != "done" || terminal["phase"] != "done" {
		t.Fatalf("terminal line = %v/%v, want done/done", terminal["event"], terminal["phase"])
	}
	if lines, ok := terminal["lines"].(map[string]any); !ok || lines["hits"] != float64(7) {
		t.Fatalf("terminal lines = %v, want hits=7", terminal["lines"])
	}
	if chunks, ok := terminal["chunks"].(map[string]any); !ok || chunks["done"] != float64(9) || chunks["total"] != float64(9) {
		t.Fatalf("terminal chunks = %v, want done=9 total=9", terminal["chunks"])
	}
	// The summary reuses the live/terminal wire keys (lines/bytes/sources/
	// chunks), matching sfu/sfl — not a tool-private search nest.
	sum, ok := last["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary block missing: %v", last)
	}
	if _, ok := sum["search"]; ok {
		t.Fatalf("summary must not nest a search block: %v", sum)
	}
	lines, _ := sum["lines"].(map[string]any)
	if lines["hits"] != float64(7) {
		t.Fatalf("summary.lines = %v, want hits=7", lines)
	}
	bytes, _ := sum["bytes"].(map[string]any)
	if bytes["read"] != float64(4000) || bytes["total"] != float64(4000) {
		t.Fatalf("summary.bytes = %v, want read=total=4000", bytes)
	}
	sources, _ := sum["sources"].(map[string]any)
	if sources["archives_done"] != float64(3) || sources["archives"] != float64(3) {
		t.Fatalf("summary.sources = %v, want archives_done=archives=3", sources)
	}
	chunks, _ := sum["chunks"].(map[string]any)
	if chunks["done"] != float64(9) || chunks["total"] != float64(9) {
		t.Fatalf("summary.chunks = %v, want done=total=9", chunks)
	}
	if truncated, ok := sum["truncated"]; ok {
		t.Fatalf("uncapped run must omit truncated: got %v", truncated)
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

// An update snapshot carries the live counters in the mapped blocks: index
// bytes while indexing, scanned bytes + chunks + hits while searching, and
// archives done/total in sources.
func TestSFSJSONOutUpdateBlocks(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.ArchivesTotal.Store(4)
	m.IndexBytesTotal.Store(800)
	m.IndexBytesDone.Store(200)

	j := &jsonOut{}
	j.setMetrics(m)
	s := j.snapshot(time.Now())
	if s.Phase != tuistat.PhaseIndexing || s.Fraction <= 0 || s.Fraction > 1 {
		t.Fatalf("index snapshot = %+v, want indexing with progress", s)
	}
	if s.Bytes == nil || s.Bytes.Read != 200 || s.Bytes.Total != 800 {
		t.Fatalf("index bytes = %+v, want read=200 total=800", s.Bytes)
	}
	if s.Chunks != nil || s.Lines != nil {
		t.Fatalf("index snapshot must carry no chunk/hit blocks: %+v", s)
	}
	if s.Sources == nil || s.Sources.Archives != 4 || s.Sources.ArchivesDone != 0 {
		t.Fatalf("index sources = %+v, want archives=4 archives_done=0", s.Sources)
	}

	m.Phase.Store(search.PhaseSearch)
	m.BytesScannedTotal.Store(8000)
	m.BytesScanned.Store(4000)
	m.ChunksTotal.Store(10)
	m.ChunksDone.Store(5)
	m.Hits.Store(3)
	m.ArchivesDone.Store(2)
	s = j.snapshot(time.Now())
	if s.Phase != tuistat.PhaseSearching {
		t.Fatalf("search snapshot phase = %q, want searching", s.Phase)
	}
	if s.Bytes == nil || s.Bytes.Read != 4000 || s.Bytes.Total != 8000 {
		t.Fatalf("search bytes = %+v, want read=4000 total=8000", s.Bytes)
	}
	if s.Chunks == nil || s.Chunks.Done != 5 || s.Chunks.Total != 10 {
		t.Fatalf("search chunks = %+v, want done=5 total=10", s.Chunks)
	}
	if s.Lines == nil || s.Lines.Hits != 3 {
		t.Fatalf("search lines = %+v, want hits=3", s.Lines)
	}
	if s.Sources == nil || s.Sources.ArchivesDone != 2 {
		t.Fatalf("search sources = %+v, want archives_done=2", s.Sources)
	}

	// A cold run (all counters zero) emits no empty {} blocks.
	cold := &search.Metrics{}
	j.setMetrics(cold)
	s = j.snapshot(time.Now())
	if s.Bytes != nil || s.Chunks != nil || s.Lines != nil || s.Sources != nil {
		t.Fatalf("cold snapshot must omit empty blocks: %+v", s)
	}
}

// The summary rollup carries the truncation flag exactly when a chunk was
// capped, mirroring the exit-code twin (COMPLETE · TRUNCATED / exit 3).
func TestSFSJSONOutSummaryStuffedCounters(t *testing.T) {
	m := &search.Metrics{}
	m.Hits.Store(42)
	m.ArchivesDone.Store(5)
	m.ArchivesTotal.Store(8)
	m.ChunksDone.Store(30)
	m.ChunksTotal.Store(60)
	m.BytesScanned.Store(1 << 20)
	m.BytesScannedTotal.Store(2 << 20)
	m.ChunksCapped.Store(1)

	j := &jsonOut{}
	j.setMetrics(m)
	b := j.summaryBlock()
	if b == nil || b.Lines == nil || b.Bytes == nil || b.Sources == nil || b.Chunks == nil {
		t.Fatalf("summary = %+v, want lines/bytes/sources/chunks", b)
	}
	if b.Lines.Hits != 42 || b.Sources.ArchivesDone != 5 || b.Sources.Archives != 8 ||
		b.Chunks.Done != 30 || b.Chunks.Total != 60 ||
		b.Bytes.Read != 1<<20 || b.Bytes.Total != 2<<20 || !b.Truncated {
		t.Fatalf("summary = %+v", b)
	}

	// Without a cap the flag stays out entirely (omitempty).
	m.ChunksCapped.Store(0)
	b = j.summaryBlock()
	if b.Truncated {
		t.Fatalf("uncapped run truncated = true, want false")
	}
	enc, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "truncated") {
		t.Fatalf("uncapped run must omit truncated on the wire: %s", enc)
	}
	if strings.Contains(string(enc), `"search"`) {
		t.Fatalf("summary must not emit a search nest: %s", enc)
	}
}

// summaryBlock must reuse the same wire keys the terminal done snapshot
// already exposes — a consumer can read one path for live and final tallies.
func TestSFSJSONOutSummarySharesTerminalWireKeys(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseDone)
	m.Hits.Store(7)
	m.ArchivesDone.Store(3)
	m.ArchivesTotal.Store(3)
	m.ChunksDone.Store(9)
	m.ChunksTotal.Store(9)
	m.BytesScanned.Store(4000)
	m.BytesScannedTotal.Store(4000)

	j := &jsonOut{}
	j.setMetrics(m)
	done := sfsStatsSnapshot(m, 0)
	sum := j.summaryBlock()
	if sum == nil {
		t.Fatal("summaryBlock returned nil")
	}
	if done.Lines == nil || sum.Lines == nil || done.Lines.Hits != sum.Lines.Hits {
		t.Fatalf("lines.hits: done=%v summary=%v", done.Lines, sum.Lines)
	}
	if done.Bytes == nil || sum.Bytes == nil ||
		done.Bytes.Read != sum.Bytes.Read || done.Bytes.Total != sum.Bytes.Total {
		t.Fatalf("bytes: done=%v summary=%v", done.Bytes, sum.Bytes)
	}
	if done.Sources == nil || sum.Sources == nil ||
		done.Sources.Archives != sum.Sources.Archives ||
		done.Sources.ArchivesDone != sum.Sources.ArchivesDone {
		t.Fatalf("sources: done=%v summary=%v", done.Sources, sum.Sources)
	}
	if done.Chunks == nil || sum.Chunks == nil ||
		done.Chunks.Done != sum.Chunks.Done || done.Chunks.Total != sum.Chunks.Total {
		t.Fatalf("chunks: done=%v summary=%v", done.Chunks, sum.Chunks)
	}

	enc, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(enc, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lines", "bytes", "sources", "chunks"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("summary wire lost %q: %v", key, wire)
		}
	}
	if _, ok := wire["search"]; ok {
		t.Fatalf("summary wire must not nest search: %v", wire)
	}
}

// A non-positive interval is a usage error when the stream is on, and inert
// while the stream is off.
func TestSFSJSONOutRejectsNonPositiveEvery(t *testing.T) {
	for _, every := range []time.Duration{0, -time.Second} {
		j, err := newJSONOut(t.TempDir()+"/stats.jsonl", every, nil)
		if err == nil || !strings.Contains(err.Error(), "must be positive") {
			t.Fatalf("every=%v: err=%v, want a positivity error", every, err)
		}
		if j != nil {
			t.Fatalf("every=%v: stream created despite error", every)
		}
	}
}

func TestSFSJSONOutOffAndBadPath(t *testing.T) {
	j, err := newJSONOut("", time.Second, nil)
	if err != nil || j.em != nil {
		t.Fatalf("empty target must be a no-op emitter, got %v/%v", j, err)
	}
	if _, err := newJSONOut(t.TempDir()+"/no-such-dir/s.jsonl", time.Second, nil); err == nil {
		t.Fatal("unopenable file must error")
	}
}

// A panic during the synchronous start (snapshot builder or write) must end
// the stream with an error terminal before the crash re-surfaces: main's
// recover hook reads joutRef, which is assigned only after start returns.
// The production provider is nil-safe, so the test injects a panicking
// builder through the seam start() consults.
func TestSFSJSONOutStartPanicEmitsErrorTerminal(t *testing.T) {
	buf := &bytes.Buffer{}
	j := &jsonOut{em: tuistat.NewEmitter(buf, time.Hour)}
	j.providerForTest = func(time.Time) tuistat.Snapshot { panic("boom") }
	func() {
		defer func() { _ = recover() }() // swallow the re-panic; the terminal is the point
		j.start()
	}()
	snaps := decodeSFSJSONL(t, buf.String())
	if len(snaps) != 2 {
		t.Fatalf("want exactly the terminal + summary lines, got %d: %v", len(snaps), snaps)
	}
	if snaps[0]["event"] != "error" || snaps[0]["tool"] != "sfs" {
		t.Fatalf("terminal = %v/%v, want error/sfs", snaps[0]["event"], snaps[0]["tool"])
	}
	if e, _ := snaps[0]["error"].(string); !strings.Contains(e, "panic:") {
		t.Fatalf("terminal error = %q, want the panic message", e)
	}
	// Even a panicked start still ends with the summary line, minimal (the
	// panicking builder is not re-entered) but identifiable.
	if snaps[1]["event"] != "summary" {
		t.Fatalf("last event = %v, want summary", snaps[1]["event"])
	}
	if _, ok := snaps[1]["summary"]; ok {
		t.Fatalf("start-panic summary must be envelope-only, got %v", snaps[1]["summary"])
	}
}

// -stats is the live screen. -json hides it only when the stream is
// stdout and stdout is a terminal.
func TestSFSJSONOutDisplayFollowsStreamTarget(t *testing.T) {
	oldTTY := stderrIsTTY
	oldStdout := stdoutIsCharDevice
	stderrIsTTY = func() bool { return true }
	stdoutIsCharDevice = func() bool { return true }
	t.Cleanup(func() {
		stderrIsTTY = oldTTY
		stdoutIsCharDevice = oldStdout
	})

	if got := resolveRunUIMode(true, "-", true, true); got != uiSilent {
		t.Fatalf("stats + stdout json on a terminal = %v, want silent", uiModeString(got))
	}
	if got := resolveRunUIMode(true, "run.jsonl", true, true); got != uiFull {
		t.Fatalf("stats + file json = %v, want full", uiModeString(got))
	}
	if got := resolveRunUIMode(false, "run.jsonl", true, true); got != uiSilent {
		t.Fatalf("file json without -stats = %v, want silent", uiModeString(got))
	}
	stdoutIsCharDevice = func() bool { return false }
	if got := resolveRunUIMode(true, "-", false, true); got != uiFull {
		t.Fatalf("stats + piped stdout json = %v, want full", uiModeString(got))
	}
	if got := resolveRunUIMode(true, "", true, true); got != uiFull {
		t.Fatalf("stats + TTY without json = %v, want full", uiModeString(got))
	}
	stderrIsTTY = func() bool { return false }
	if got := resolveRunUIMode(true, "", true, true); got != uiSilent {
		t.Fatalf("stats + no stderr TTY = %v, want silent", uiModeString(got))
	}
}
