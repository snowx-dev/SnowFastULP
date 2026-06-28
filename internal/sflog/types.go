package sflog

type Credential struct {
	URL      string
	Username string
	Password string
	Source   string
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
	IssueNoULP
	// IssueMissingVolume marks a multi-volume RAR continuation part (e.g.
	// name.part2.rar) whose first volume (name.part1.rar) was not present, so
	// the set cannot be opened. Surfaced as a skip, not a failure.
	IssueMissingVolume
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
	case IssueNoULP:
		return "no-ulp"
	case IssueMissingVolume:
		return "missing-volume"
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
	// an isolated problem (e.g. a nested archive whose password was not found),
	// so -del retains it rather than discarding un-extracted data.
	HadIssue bool
}

type ExtractStats struct {
	FilesScanned    int
	ArchivesScanned int
	Logs            int // distinct logs (top-level subfolder or archive)
	Credentials     int
	Emitted         int
	Duplicates      int
	SkippedFiles    int
	SkippedArchives int

	// granular issue counters for the summary
	PasswordNotFound int
	ParseErrors      int
	OpenErrors       int
	NoULP            int
	MissingVolumes   int

	// capped, ordered list of concrete problems (see issueCap)
	Issues []Issue
}
