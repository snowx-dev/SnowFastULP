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

	lines := renderPhase0Lines(time.Second, &ulpengine.Metrics{}, r, 100, 100, 0, 86)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "[1/3 LIBRARY PREP]") {
		t.Errorf("want [1/3 LIBRARY PREP] header, got:\n%s", joined)
	}
	// no frozen-0% shard stat block (was the bug)
	if strings.Contains(joined, "chunks ") || strings.Contains(joined, "shard ") {
		t.Errorf("shard panel leaked into phase 0:\n%s", joined)
	}
	if !strings.Contains(joined, "Destination dedup") {
		t.Errorf("missing OD frame:\n%s", joined)
	}
}
