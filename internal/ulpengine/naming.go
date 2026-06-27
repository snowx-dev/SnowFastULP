package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultBasename is the per-run output stem ("sfu_<stamp>.txt"). The chunked
// zstd sink reuses it for _partN names, and the command builds the initial
// output path from it, so it must be a single source of truth.
func DefaultBasename(stamp string) string {
	return "sfu_" + stamp + ".txt"
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

// EnsureDestDedupMetrics lazily allocates the phase-0 / output-index metric
// blocks on a resolved -od run so the TUI and pipeline share the same counters.
func EnsureDestDedupMetrics(r *Resolved) {
	if r == nil || !r.Cfg.DestDedup {
		return
	}
	if r.OdMetrics == nil {
		r.OdMetrics = &ODMetrics{}
	}
	if r.OutputIdxMetrics == nil {
		r.OutputIdxMetrics = &ODMetrics{}
	}
}

// EstimateDestKeyBytes peeks sidecar headers under destDir and sums 8 B x keys,
// shared by preflight and the adaptive bucket sizer so both see the same
// library footprint. Unreadable sidecar = archive_size/10 fallback;
// excludeStamp filters out the current run's own in-progress archive.
func EstimateDestKeyBytes(destDir, excludeStamp string) int64 {
	if destDir == "" {
		return 0
	}
	matches, err := filepath.Glob(filepath.Join(destDir, "sfu_*.txt.zst"))
	if err != nil {
		return 0
	}
	var total int64
	for _, p := range matches {
		runID, _ := parseArchiveName(p)
		if runID == "" || runID == excludeStamp {
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
