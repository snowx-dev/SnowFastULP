package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
)

// normalizeRunStamp trims at most one leading "sfu_" so a caller passing the
// parseArchiveName form ("sfu_<stamp>") and one passing the bare stamp produce
// identical archive names and run IDs.
func normalizeRunStamp(stamp string) string {
	return strings.TrimPrefix(stamp, "sfu_")
}

// archiveRunID returns the current run's ID exactly as parseArchiveName
// reports it for the run's output archive ("sfu_" plus the stamp with at most
// one leading "sfu_" trimmed). Every run-ID comparison — destination discovery
// exclusion, bucket sizing — must use this form; comparing the raw caller
// stamp never matches ("sfu_x" vs "x") and makes a retry's own previous
// archive a dedup source that is then overwritten, dropping records the retry
// input still contained.
func archiveRunID(stamp string) string {
	return "sfu_" + normalizeRunStamp(stamp)
}

// DefaultBasename is the per-run output stem ("sfu_<stamp>.txt"). The chunked
// zstd sink reuses it for _partN names, and the command builds the initial
// output path from it, so it must be a single source of truth. Normalizing the
// stamp keeps DefaultBasename idempotent: bare and sfu_-prefixed caller stamps
// name the same output archive.
func DefaultBasename(stamp string) string {
	return "sfu_" + normalizeRunStamp(stamp) + ".txt"
}

// WithZstExt appends .zst when compressing and not already suffixed.
func WithZstExt(p string, compress bool) string {
	if !compress {
		return p
	}
	if strings.EqualFold(filepath.Ext(p), ".zst") {
		return p
	}
	return p + ".zst"
}

// EnsureDestDedupMetrics lazily allocates the phase-0 metric block on a
// resolved -od run so the TUI and pipeline share the same counters.
func EnsureDestDedupMetrics(r *Resolved) {
	if r == nil || !r.Cfg.DestDedup {
		return
	}
	if r.OdMetrics == nil {
		r.OdMetrics = &ODMetrics{}
	}
}

// EstimateDestKeyBytes peeks sidecar headers under destDir and sums 8 B x keys,
// shared by preflight and the adaptive bucket sizer so both see the same
// library footprint. Unreadable sidecar = archive_size/10 fallback;
// excludeRunID filters out the current run's own in-progress archive. It is
// normalized through archiveRunID, so callers may pass the bare stamp or the
// parseArchiveName form.
func EstimateDestKeyBytes(destDir, excludeRunID string) int64 {
	if destDir == "" {
		return 0
	}
	if excludeRunID != "" {
		excludeRunID = archiveRunID(excludeRunID)
	}
	matches, err := listLibraryArchives(destDir)
	if err != nil {
		// estimator tolerance unchanged from the Glob era: an unreadable or
		// not-yet-existing library estimates 0; the real discovery pass in
		// discoverArchiveRuns still fails loud on real errors
		return 0
	}
	var total int64
	for _, p := range matches {
		runID, _ := parseArchiveName(p)
		if runID == "" || runID == excludeRunID {
			continue
		}
		side := sidecarPathForArchive(p)
		if hdr, err := readSidecarHeader(side); err == nil {
			total += int64(hdr.keyCount) * SidecarKeyBytes
			continue
		}
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size() / 10
		}
	}
	return total
}
