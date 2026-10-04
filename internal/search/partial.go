package search

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// maxPartialExamples caps how many individual failures are listed in a
// PartialScanError message. The exact total count is always preserved in the
// message, so a 500-file failure is summarized, never truncated invisibly.
const maxPartialExamples = 10

// PartialFailure records one failed scan unit: an archive that could not be
// indexed or opened, a plain-text file that could not be read, or a chunk
// whose zstd decode failed. ChunkID is negative when the failure is not tied
// to a specific chunk (index build, txt open/read, decoder setup).
type PartialFailure struct {
	Path    string
	ChunkID int
	Err     error
}

// PartialScanError aggregates per-file/per-chunk scan failures. It is returned
// only AFTER every healthy hit has been emitted: per-item continuation stays,
// so one corrupt archive never hides valid hits — but the run must not look
// COMPLETE when any selected file or chunk could not be scanned. Deliberate
// truncation (the -l hit cap, the per-chunk cap) is NOT recorded here.
//
// The message is deterministic: failures are sorted by path, then chunk, and
// at most maxPartialExamples are listed with the exact total count.
type PartialScanError struct {
	Failures []PartialFailure
}

func (e *PartialScanError) Error() string {
	fs := make([]PartialFailure, len(e.Failures))
	copy(fs, e.Failures)
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Path != fs[j].Path {
			return fs[i].Path < fs[j].Path
		}
		return fs[i].ChunkID < fs[j].ChunkID
	})

	var b strings.Builder
	if len(fs) <= maxPartialExamples {
		fmt.Fprintf(&b, "incomplete scan: %d failure(s):", len(fs))
	} else {
		fmt.Fprintf(&b, "incomplete scan: %d failure(s) (showing %d):", len(fs), maxPartialExamples)
	}
	for i, f := range fs {
		if i == maxPartialExamples {
			break
		}
		if f.ChunkID >= 0 {
			fmt.Fprintf(&b, "\n  %s: chunk %d: %v", f.Path, f.ChunkID, f.Err)
		} else {
			fmt.Fprintf(&b, "\n  %s: %v", f.Path, f.Err)
		}
	}
	return b.String()
}

// Unwrap exposes every underlying failure so errors.Is/As can reach causes.
func (e *PartialScanError) Unwrap() []error {
	errs := make([]error, len(e.Failures))
	for i, f := range e.Failures {
		errs[i] = f.Err
	}
	return errs
}

// FailureCollector accumulates scan failures thread-safely from worker
// goroutines. Empty errors are never recorded.
type FailureCollector struct {
	mu sync.Mutex
	fs []PartialFailure
}

// Add records one failed scan unit. err == nil is ignored.
func (c *FailureCollector) Add(path string, chunkID int, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.fs = append(c.fs, PartialFailure{Path: path, ChunkID: chunkID, Err: err})
	c.mu.Unlock()
}

// Result returns the collected failures as a *PartialScanError, or nil when
// nothing failed.
func (c *FailureCollector) Result() *PartialScanError {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.fs) == 0 {
		return nil
	}
	fs := make([]PartialFailure, len(c.fs))
	copy(fs, c.fs)
	return &PartialScanError{Failures: fs}
}
