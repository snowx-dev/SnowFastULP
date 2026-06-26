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
)

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
	NoULP            int

	// capped, ordered list of concrete problems (see issueCap)
	Issues []Issue
}
