package tuistat

import (
	"encoding/json"
	"testing"
)

// The v1 wire schema is a contract with wrapper scripts: serialized key
// names must match DESIGN.md exactly, independent of internal Go field
// names. This test pins the names that have drifted once.
func TestSnapshotWireKeyNames(t *testing.T) {
	s := Snapshot{
		Bytes:   &BytesBlock{Read: 9, Written: 10, BPS: 11.5},
		Buckets: &BucketsBlock{DoneTotal: DoneTotal{Done: 1, Total: 2}, BytesDone: 3, BytesTotal: 4},
		Workers: &WorkersBlock{
			Busy: 1, Total: 2,
			Active: []WorkerRow{{Path: "a.zip", PartIdx: 1, BytesDone: 21}},
		},
		Library: &LibraryBlock{KeysEstimate: 5, RegenBytesDone: 6, RegenBytesTotal: 7, RegenBPS: 8.5},
		History: &HistoryBlock{Enabled: true, BytesDone: 30, BytesTotal: 31, Checked: 33, Skipped: 34},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}

	bk, ok := wire["buckets"].(map[string]any)
	if !ok {
		t.Fatalf("buckets block missing: %s", b)
	}
	if _, ok := bk["bytes_read"]; !ok {
		t.Fatalf("buckets must serialize bytes_read (contract), got %v", bk)
	}
	if _, ok := bk["bytes_done"]; ok {
		t.Fatalf("buckets must not serialize the old bytes_done key: %v", bk)
	}
	if _, ok := bk["bytes_total"]; !ok {
		t.Fatalf("buckets lost bytes_total: %v", bk)
	}

	lib, ok := wire["library"].(map[string]any)
	if !ok {
		t.Fatalf("library block missing: %s", b)
	}
	for _, k := range []string{"keys_estimate", "regen_bytes_read", "regen_bytes_total", "regen_bps"} {
		if _, ok := lib[k]; !ok {
			t.Fatalf("library lost %q (contract name): %v", k, lib)
		}
	}
	if _, ok := lib["regen_bytes_done"]; ok {
		t.Fatalf("library must not serialize the old regen_bytes_done key: %v", lib)
	}

	by, ok := wire["bytes"].(map[string]any)
	if !ok {
		t.Fatalf("bytes block missing: %s", b)
	}
	for _, k := range []string{"read", "written", "bps"} {
		if _, ok := by[k]; !ok {
			t.Fatalf("bytes lost %q: %v", k, by)
		}
	}

	wr, ok := wire["workers"].(map[string]any)
	if !ok {
		t.Fatalf("workers block missing: %s", b)
	}
	rows, ok := wr["active"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("workers active rows = %v", wr["active"])
	}
	row, _ := rows[0].(map[string]any)
	for _, k := range []string{"path", "part_idx", "bytes_done"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("worker row lost %q: %v", k, row)
		}
	}

	hi, ok := wire["history"].(map[string]any)
	if !ok {
		t.Fatalf("history block missing: %s", b)
	}
	for _, k := range []string{"enabled", "bytes_done", "bytes_total", "checked", "skipped"} {
		if _, ok := hi[k]; !ok {
			t.Fatalf("history lost %q (contract name): %v", k, hi)
		}
	}
}

// The final "summary" event's rollup is a contract too: wrapper scripts read
// these snake_case keys for unique/reject totals and the per-reason
// breakdown, so they are pinned the same way as the live schema.
func TestSummaryWireKeyNames(t *testing.T) {
	s := Snapshot{
		Event: EventSummary,
		Summary: &SummaryBlock{
			Lines:     &LinesBlock{Read: 1, Accepted: 2, Rejected: 3, Unique: 4, Dupes: 5, InLibrary: 6, Unrepresentable: 7, Hits: 27},
			Rejects:   &SummaryRejects{Total: 8, Dupes: 5, InLibrary: 6, Unrepresentable: 7, TooLong: 9, Malformed: 10, PasswordTooLong: 12, LibraryRefused: 11},
			Bytes:     &BytesBlock{Read: 12, Written: 13, Total: 28},
			Sources:   &SourcesBlock{Files: 14, Archives: 15, Skipped: 16, Deleted: 17, ArchivesDone: 29},
			Chunks:    &DoneTotal{Done: 30, Total: 31},
			History:   &HistoryBlock{Checked: 18, Skipped: 19},
			Env:       &EnvBlock{Copied: 20, DirsCopied: 21, SkippedOverCap: 22, DirsSkippedOverCap: 23},
			Library:   &SummaryLibrary{Lines: 24, Added: 25, UpgradedParts: 26},
			Output:    &OutputBlock{Paths: []string{"out.txt"}},
			Truncated: true,
		},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Event     string         `json:"event"`
		ElapsedMS int64          `json:"elapsed_ms"`
		Summary   map[string]any `json:"summary"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Event != EventSummary {
		t.Fatalf("event = %q, want %q", wire.Event, EventSummary)
	}
	// Elapsed time lives on the line's envelope only; the summary block must
	// not duplicate it. sfs shares lines/bytes/sources/chunks with the live
	// schema — there is no tool-private "search" nest.
	for _, key := range []string{"lines", "rejects", "bytes", "sources", "chunks", "history", "env", "library", "output", "truncated"} {
		if _, ok := wire.Summary[key]; !ok {
			t.Fatalf("summary lost %q: %v", key, wire.Summary)
		}
	}
	if _, ok := wire.Summary["search"]; ok {
		t.Fatalf("summary must not nest a search block: %v", wire.Summary)
	}
	if _, ok := wire.Summary["elapsed_ms"]; ok {
		t.Fatalf("summary must not carry its own elapsed_ms (envelope has it): %v", wire.Summary)
	}
	lines := wire.Summary["lines"].(map[string]any)
	for _, k := range []string{"read", "accepted", "rejected", "unique", "dupes", "in_library", "unrepresentable", "hits"} {
		if _, ok := lines[k]; !ok {
			t.Fatalf("summary lines lost %q: %v", k, lines)
		}
	}
	rej := wire.Summary["rejects"].(map[string]any)
	for _, k := range []string{"total", "dupes", "in_library", "unrepresentable", "too_long", "malformed", "password_too_long", "library_refused"} {
		if _, ok := rej[k]; !ok {
			t.Fatalf("summary rejects lost %q: %v", k, rej)
		}
	}
	src := wire.Summary["sources"].(map[string]any)
	for _, k := range []string{"files", "archives", "archives_done", "skipped", "deleted"} {
		if _, ok := src[k]; !ok {
			t.Fatalf("summary sources lost %q: %v", k, src)
		}
	}
	chunks := wire.Summary["chunks"].(map[string]any)
	for _, k := range []string{"done", "total"} {
		if _, ok := chunks[k]; !ok {
			t.Fatalf("summary chunks lost %q: %v", k, chunks)
		}
	}
	bytes := wire.Summary["bytes"].(map[string]any)
	for _, k := range []string{"read", "written", "total"} {
		if _, ok := bytes[k]; !ok {
			t.Fatalf("summary bytes lost %q: %v", k, bytes)
		}
	}
	lib := wire.Summary["library"].(map[string]any)
	for _, k := range []string{"lines", "added", "upgraded_parts"} {
		if _, ok := lib[k]; !ok {
			t.Fatalf("summary library lost %q: %v", k, lib)
		}
	}
	env := wire.Summary["env"].(map[string]any)
	for _, k := range []string{"copied", "dirs_copied", "skipped_over_cap", "dirs_skipped_over_cap"} {
		if _, ok := env[k]; !ok {
			t.Fatalf("summary env lost %q: %v", k, env)
		}
	}
}
