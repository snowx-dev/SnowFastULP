package main

import (
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// phase-0 TUI: OD frame is primary, no frozen-0% shard panel
func TestRenderPhase0LinesIsPrimary(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	r.Cfg.DestDedup = true
	r.OdMetrics = &ulpengine.ODMetrics{}
	r.OdMetrics.Phase.Store(int32(ulpengine.ODPhaseRegen))
	r.OdMetrics.ArchivesTotal.Store(5)
	r.OdMetrics.KeysTotalEstimate.Store(2_400_000)

	lines := renderPhase0Lines(time.Second, &ulpengine.Metrics{}, r, 100, 100, 0, 86)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "[1/2 LIBRARY PREP]") {
		t.Errorf("want [1/2 LIBRARY PREP] header, got:\n%s", joined)
	}
	// no frozen-0% shard stat block (was the bug)
	if strings.Contains(joined, "chunks ") || strings.Contains(joined, "shard ") {
		t.Errorf("shard panel leaked into phase 0:\n%s", joined)
	}
	if !strings.Contains(joined, "one-time re-index") {
		t.Errorf("missing OD frame with re-index orientation tag:\n%s", joined)
	}
	if strings.Contains(joined, "updating library") {
		t.Errorf("duplicated 'updating library' must not render under the Library-labeled header:\n%s", joined)
	}
	if !strings.Contains(joined, " lines") {
		t.Errorf("missing relocated library total (lines unit):\n%s", joined)
	}
}

// phase-0 primary: the system RAM/CPU row must sit INSIDE the OD box like
// the phase 1/2 frames, never appended outside the gradientBox
func TestRenderPhase0LinesSystemRowInsideFrame(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	r.Cfg.DestDedup = true
	r.OdMetrics = &ulpengine.ODMetrics{}
	r.OdMetrics.Phase.Store(int32(ulpengine.ODPhaseRegen))
	r.OdMetrics.ArchivesTotal.Store(5)
	r.OdMetrics.RegenBytesTotal.Store(1 << 30)
	r.OdMetrics.RegenBytesRead.Store(1 << 28)

	lines := renderPhase0Lines(time.Second, &ulpengine.Metrics{}, r, 100, 100, 0, 86)
	sysIdx, boxTop, boxBottom := -1, -1, -1
	for i, ln := range lines {
		plain := stripANSI(ln)
		switch {
		case strings.Contains(plain, "System"):
			sysIdx = i
		case strings.Contains(plain, "╭"):
			boxTop = i
		case strings.Contains(plain, "╰"):
			boxBottom = i
		}
	}
	if sysIdx < 0 {
		t.Fatalf("phase-0 frame missing System row\nlines:\n%s", strings.Join(lines, "\n"))
	}
	if boxTop < 0 || boxBottom < 0 || sysIdx <= boxTop || sysIdx >= boxBottom {
		t.Fatalf("System row not inside the OD box (top=%d, sys=%d, bottom=%d)\nlines:\n%s",
			boxTop, sysIdx, boxBottom, strings.Join(lines, "\n"))
	}
	if !strings.Contains(stripANSI(lines[sysIdx]), "│") {
		t.Errorf("System row rendered without box borders:\n%s", lines[sysIdx])
	}
}
