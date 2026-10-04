package sflog

import "github.com/snowx-dev/SnowFastULP/internal/history"

type Credential struct {
	URL      string
	Username string
	Password string
	Source   string
}

type AmbiguityCounts struct {
	Total          int
	PathOrPassword int
	PortOrLogin    int
}

type SourceFile struct {
	Path string
}

type WriteStats struct {
	Seen       int
	Emitted    int
	Duplicates int
}

// IssueKind categorises a per-source problem surfaced in the final summary.
type IssueKind int

const (
	IssuePasswordNotFound IssueKind = iota
	IssueParseError
	IssueOpenError
	// IssueReadError marks a source (a loose file or an archive member) that
	// could not be fully read. The source may still parse, but its content
	// was not fully verified, so -del keeps it.
	IssueReadError
	IssueNoULP
	// IssueMissingVolume marks a multi-volume RAR continuation part (e.g.
	// name.part2.rar) whose first volume (name.part1.rar) was not present, so
	// the set cannot be opened. Surfaced as a skip, not a failure.
	IssueMissingVolume
	// IssueEnvCopy marks a Telegram tdata (or other -env dir) copy/promote
	// failure. The source parsed; -del must keep it because the dest tree
	// did not land.
	IssueEnvCopy
	// IssueMixedFormat marks a credential source that contained BOTH labeled
	// Key: value blocks and valid label-less url:user:password lines. Labeled
	// precedence is kept (output unchanged), but the discarded colon lines
	// would be lost, so -del keeps the source and the run is not recorded as
	// history-complete (review H-20).
	IssueMixedFormat
)

// String returns a stable, log-friendly slug for the issue kind.
func (k IssueKind) String() string {
	switch k {
	case IssuePasswordNotFound:
		return "password-not-found"
	case IssueParseError:
		return "parse-error"
	case IssueOpenError:
		return "open-error"
	case IssueReadError:
		return "read-error"
	case IssueNoULP:
		return "no-ulp"
	case IssueMissingVolume:
		return "missing-volume"
	case IssueEnvCopy:
		return "env-copy"
	case IssueMixedFormat:
		return "mixed-format"
	default:
		return "unknown"
	}
}

// Issue records a single non-fatal problem tied to a source path so the
// summary can tell the analyst exactly what was skipped and why.
type Issue struct {
	Path string
	Kind IssueKind
	Err  error
}

// SourceResult reports the parse outcome for one discovered source (a loose
// credential file or an archive). Callers use it to decide -del eligibility.
type SourceResult struct {
	Path      string
	IsArchive bool
	OK        bool
	// HadIssue is set when the source parsed without a fatal error but recorded
	// an isolated problem (e.g. a nested archive whose password was not found,
	// or parse-quality rejects/mixed-format discards), so the run classifies
	// the source as failed for exit codes and the summary.
	HadIssue bool
	// HistoryComplete records whether the source was fully extracted and read.
	// Parse-quality outcomes (gate rejects, mixed-format discards) count as
	// complete since 2026-09-30 — rejected lines are deterministic — and so do
	// extraction failures (open/read/password/missing-volume/env-copy): failed
	// sources are recorded complete and are NOT retried under -history (the
	// same decision; see cmd/sfl/main.go commitHistory). Only interrupted runs
	// record nothing. A fully-read source with no credentials is complete
	// even though HadIssue is true.
	HistoryComplete  bool
	HistoryCandidate history.Candidate
	HistoryHit       bool
}

type ExtractStats struct {
	FilesScanned            int
	ArchivesScanned         int
	Logs                    int // distinct logs (top-level subfolder or archive)
	Credentials             int
	AmbiguityTotal          int
	AmbiguityPathOrPassword int
	AmbiguityPortOrLogin    int
	Emitted                 int
	Duplicates              int
	SkippedFiles            int
	SkippedArchives         int
	HistoryChecked          int
	HistorySkipped          int

	// granular issue counters for the summary
	PasswordNotFound int
	ParseErrors      int
	OpenErrors       int
	ReadErrors       int
	NoULP            int
	MissingVolumes   int

	// FailedSources counts sources with any recorded problem (a failed parse
	// or an isolated issue): the summary Health row's Failed segment.
	// Populated by cmd/sfl from the per-source results, not by the engine.
	FailedSources int
	// DeletedSources / PreservedSources are the -del transparency counters:
	// how many source units -del removed, and how many failing sources were
	// deliberately kept. Populated by cmd/sfl after deletion runs.
	DeletedSources   int
	PreservedSources int

	// Env copy counters (-env). Populated from EnvCopier.Close().
	EnvCopied          int
	EnvDeduped         int
	EnvSkippedOverCap  int
	EnvWriteErrors     int
	EnvOpenErrors      int
	EnvReadErrors      int
	EnvWriteFailures   int
	EnvCollisionErrors int
	EnvTdataErrors     int
	// EnvDirsCopied counts Telegram tdata folders copied whole under -env
	// (loose on-disk or promoted from archive staging). Kept separate from
	// EnvCopied (flat env/key files only).
	EnvDirsCopied int
	// EnvDirsSkippedOverCap counts tdata folders skipped for exceeding
	// TdataCopyMaxBytes. Distinct from EnvSkippedOverCap (flat env/key files).
	EnvDirsSkippedOverCap int

	// capped, ordered list of concrete problems (see issueCap)
	Issues []Issue
}
