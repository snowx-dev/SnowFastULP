package tuistat

import "time"

// Snapshot is one live-stats sample for the -json stream. It is a
// superset of what the sfl and sfu TUIs render: each tool fills only the
// blocks that apply (nil blocks are omitted), so one schema serves both.
// The envelope (v, event, tool, ts, elapsed_ms) is stamped by the Emitter;
// providers fill everything else.
type Snapshot struct {
	V         int    `json:"v"`
	Event     string `json:"event"`
	Tool      string `json:"tool,omitempty"`
	Ts        string `json:"ts"`
	ElapsedMS int64  `json:"elapsed_ms"`

	// Phase is a normalized lifecycle name; Status is the human TUI label
	// when it adds information beyond the phase (e.g. "merging…").
	Phase    string  `json:"phase,omitempty"`
	Status   string  `json:"status,omitempty"`
	Fraction float64 `json:"fraction,omitempty"`
	DryRun   bool    `json:"dry_run,omitempty"`
	Error    string  `json:"error,omitempty"`
	// Code rides terminal "error" events only: the process exit code the run
	// will end with (internal/exitcode). Additive for stream consumers;
	// omitted on every other event.
	Code int `json:"code,omitempty"`

	Bytes   *BytesBlock   `json:"bytes,omitempty"`
	Lines   *LinesBlock   `json:"lines,omitempty"`
	Sources *SourcesBlock `json:"sources,omitempty"`
	Chunks  *DoneTotal    `json:"chunks,omitempty"`
	Buckets *BucketsBlock `json:"buckets,omitempty"`
	Workers *WorkersBlock `json:"workers,omitempty"`
	Library *LibraryBlock `json:"library,omitempty"`
	Env     *EnvBlock     `json:"env,omitempty"`
	History *HistoryBlock `json:"history,omitempty"`
	Output  *OutputBlock  `json:"output,omitempty"`

	// Summary carries the end-of-run rollup on the final "summary" event
	// only; live start/update lines never set it.
	Summary *SummaryBlock `json:"summary,omitempty"`
}

// Stream event names.
const (
	EventStart       = "start"
	EventUpdate      = "update"
	EventDone        = "done"
	EventInterrupted = "interrupted"
	EventError       = "error"
	// EventSummary is the final line of every stream: an end-of-run rollup
	// mirroring the binary's human summary, written after the terminal event.
	EventSummary = "summary"
)

// Normalized phase names shared by sfl and sfu.
const (
	PhaseScanning          = "scanning"
	PhaseCheckingHistory   = "checking-history"
	PhaseExtracting        = "extracting"
	PhaseParsing           = "parsing"
	PhasePreparingLibrary  = "preparing-library"
	PhaseUpgradingLibrary  = "upgrading-library"
	PhaseReadingCredential = "reading-credentials"
	PhaseIngesting         = "ingesting"
	PhaseDeduping          = "deduping"
	PhaseDone              = "done"

	// sfs search phases (additive): index builds sidecars, search scans them.
	PhaseIndexing  = "indexing"
	PhaseSearching = "searching"
)

// DoneTotal is the ubiquitous N-of-M counter pair.
type DoneTotal struct {
	Done  int64 `json:"done,omitempty"`
	Total int64 `json:"total,omitempty"`
}

type BytesBlock struct {
	Read    int64   `json:"read,omitempty"`
	Total   int64   `json:"total,omitempty"`
	BPS     float64 `json:"bps,omitempty"`
	Shard   int64   `json:"shard,omitempty"`
	Written int64   `json:"written,omitempty"`
}

type LinesBlock struct {
	Read            int64 `json:"read,omitempty"`
	Accepted        int64 `json:"accepted,omitempty"`
	Rejected        int64 `json:"rejected,omitempty"`
	Unique          int64 `json:"unique,omitempty"`
	Dupes           int64 `json:"dupes,omitempty"`
	InLibrary       int64 `json:"in_library,omitempty"`
	Unrepresentable int64 `json:"unrepresentable,omitempty"`
	// Hits is the sfs search counter: raw hits emitted so far. The other
	// line counters are sfu/sfl parse tallies sfs does not track; they stay
	// omitted on sfs lines.
	Hits int64 `json:"hits,omitempty"`
}

type SourcesBlock struct {
	Files      int64 `json:"files,omitempty"`
	Archives   int64 `json:"archives,omitempty"`
	LogsDone   int64 `json:"logs_done,omitempty"`
	LogsTotal  int64 `json:"logs_total,omitempty"`
	Discovered int64 `json:"discovered,omitempty"`
	// Skipped/Deleted are summary-only tallies: sources dropped unread
	// (sfl skipped files+archives) and inputs removed after a successful
	// parse (sfu -del).
	Skipped int64 `json:"skipped,omitempty"`
	Deleted int64 `json:"deleted,omitempty"`
	// ArchivesDone is the sfs live counter: search-completed archives next
	// to the total in Archives. Additive; other tools never set it.
	ArchivesDone int64 `json:"archives_done,omitempty"`
}

type BucketsBlock struct {
	DoneTotal
	// BytesDone serializes as "bytes_read" per the design contract: the
	// bucket bar tracks bytes READ from the bucket datasets.
	BytesDone  int64 `json:"bytes_read,omitempty"`
	BytesTotal int64 `json:"bytes_total,omitempty"`
}

type WorkersBlock struct {
	Busy   int32       `json:"busy,omitempty"`
	Total  int32       `json:"total,omitempty"`
	Active []WorkerRow `json:"active,omitempty"`
}

// WorkerRow is one live worker: an extraction/parsing slot (sfl) or an OD
// regen/index row (both tools' ingest panels).
type WorkerRow struct {
	Path       string `json:"path,omitempty"`
	Stage      string `json:"stage,omitempty"`
	PartIdx    int32  `json:"part_idx,omitempty"`
	PartsTotal int32  `json:"parts_total,omitempty"`
	BytesDone  int64  `json:"bytes_done,omitempty"`
	BytesTotal int64  `json:"bytes_total,omitempty"`
}

type LibraryBlock struct {
	KeysEstimate      int64   `json:"keys_estimate,omitempty"`
	KeysLoaded        int64   `json:"keys_loaded,omitempty"`
	Archives          int32   `json:"archives,omitempty"`
	FilesTotal        int32   `json:"files_total,omitempty"`
	PartsRegenDone    int32   `json:"parts_regen_done,omitempty"`
	PartsRegenTotal   int32   `json:"parts_regen_total,omitempty"`
	PartsUpgradeTotal int32   `json:"parts_upgrade_total,omitempty"`
	RegenBytesDone    int64   `json:"regen_bytes_read,omitempty"`
	RegenBytesTotal   int64   `json:"regen_bytes_total,omitempty"`
	RegenBPS          float64 `json:"regen_bps,omitempty"`
}

type EnvBlock struct {
	Enabled bool  `json:"enabled,omitempty"`
	Copied  int64 `json:"copied,omitempty"`
	Deduped int64 `json:"deduped,omitempty"`
	// Summary-only env tallies the sfl recap renders when non-zero: whole
	// tdata folders copied, and files/folders dropped over the size caps.
	DirsCopied         int64 `json:"dirs_copied,omitempty"`
	SkippedOverCap     int64 `json:"skipped_over_cap,omitempty"`
	DirsSkippedOverCap int64 `json:"dirs_skipped_over_cap,omitempty"`
}

// SummaryBlock is the end-of-run rollup the final "summary" event carries —
// the JSON twin of each binary's human end-of-run summary. Every block is
// optional: each tool/phase fills only what applies. For per-reason numbers
// see SummaryRejects; the reuse of LinesBlock/SourcesBlock/Chunks/etc. keeps
// the wire keys identical to the live schema (sfu, sfl, and sfs). Elapsed
// time stays on the line's envelope (Snapshot.ElapsedMS, stamped by the
// Emitter) — the summary line is self-contained without duplicating it here.
type SummaryBlock struct {
	Lines   *LinesBlock     `json:"lines,omitempty"`
	Rejects *SummaryRejects `json:"rejects,omitempty"`
	Bytes   *BytesBlock     `json:"bytes,omitempty"`
	Sources *SourcesBlock   `json:"sources,omitempty"`
	Chunks  *DoneTotal      `json:"chunks,omitempty"`
	History *HistoryBlock   `json:"history,omitempty"`
	Env     *EnvBlock       `json:"env,omitempty"`
	Library *SummaryLibrary `json:"library,omitempty"`
	Output  *OutputBlock    `json:"output,omitempty"`
	// Truncated is set by sfs when -max-hits-per-chunk cut one or more
	// chunks (exit-code twin of COMPLETE · TRUNCATED / exit 3).
	Truncated bool `json:"truncated,omitempty"`
}

// SummaryRejects breaks the run's drops down per reason. Total is the
// "Removed"-style rollup the human summaries show (rejects + in-run dupes +
// already-in-library); each reason is the count behind one summary bullet.
type SummaryRejects struct {
	Total           int64 `json:"total,omitempty"`
	Dupes           int64 `json:"dupes,omitempty"`
	InLibrary       int64 `json:"in_library,omitempty"`
	Unrepresentable int64 `json:"unrepresentable,omitempty"`
	// TooLong/Malformed are the sfu parse rejects behind the rejected total:
	// oversized lines vs lines no parser rule accepted. Unrepresentable is
	// also a reject but gets its own key (it parsed cleanly).
	TooLong   int64 `json:"too_long,omitempty"`
	Malformed int64 `json:"malformed,omitempty"`
	// PasswordTooLong is the subset of the sfu parse rejects refused
	// specifically by the 64-char password cap (tagged password>64 in
	// -debug-reject): named because it is otherwise indistinguishable from
	// generic malformed and costs a bisect to diagnose. Disjoint from
	// Malformed, so the per-reason tallies still sum into the reject total.
	PasswordTooLong int64 `json:"password_too_long,omitempty"`
	// LibraryRefused is the sfl -od ingest drop: unique credentials the
	// destination library's parser refused (non-ULP).
	LibraryRefused int64 `json:"library_refused,omitempty"`
}

// SummaryLibrary reports the destination library after a -od/-odr run: the
// total indexed line count, what this run added (would add, in a dry run),
// and the one-time index upgrade when it happened.
type SummaryLibrary struct {
	Lines         int64 `json:"lines,omitempty"`
	Added         int64 `json:"added,omitempty"`
	UpgradedParts int32 `json:"upgraded_parts,omitempty"`
}

// HistoryBlock is the live checking-history progress: bytes_done/bytes_total
// reflect the sources' stat sizes (under the sampled fingerprint each source
// reports its full size as soon as its head/tail samples are read, so the
// phase completes near-instantly), checked counts the sources fingerprinted
// so far, and skipped the sources the history database reports as already
// completed. The same block rides the summary event's history rollup
// (checked/skipped).
type HistoryBlock struct {
	Enabled    bool  `json:"enabled,omitempty"`
	BytesDone  int64 `json:"bytes_done,omitempty"`
	BytesTotal int64 `json:"bytes_total,omitempty"`
	Checked    int64 `json:"checked,omitempty"`
	Skipped    int64 `json:"skipped,omitempty"`
}

type OutputBlock struct {
	Paths []string `json:"paths,omitempty"`
}

// RateSampler turns monotonic counters into per-second rates, one sampler per
// counter. Both the TUI monitors and the -json providers use it; each
// consumer owns its own sampler so they never share mutable state.
type RateSampler struct {
	prevAt time.Time
	prev   int64
}

// Rate returns bytes-per-second (or lines-per-second) since the previous
// call. The first call returns 0 and only primes the sampler.
func (r *RateSampler) Rate(cur int64, now time.Time) float64 {
	if r.prevAt.IsZero() {
		r.prevAt, r.prev = now, cur
		return 0
	}
	dt := now.Sub(r.prevAt).Seconds()
	if dt < 0.05 {
		return 0
	}
	bps := float64(cur-r.prev) / dt
	r.prevAt, r.prev = now, cur
	return bps
}

// Rates bundles the per-phase throughputs a snapshot builder passes through;
// each is optional (0 = unknown or not advancing).
type Rates struct {
	Read  float64 `json:"read,omitempty"`
	Shard float64 `json:"shard,omitempty"`
	Write float64 `json:"write,omitempty"`
	Regen float64 `json:"regen,omitempty"`
}
