package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

func TestRenderODFrameDiscoverShowsLegacyHint(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseDiscover))
	m.ArchivesTotal.Store(5)
	m.PartsUpgradeTotal.Store(12)

	lines := renderODFrame(m, 0, 86)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"legacy index detected",
		"one-time upgrade next",
		"Legacy index format",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("discover legacy hint missing %q\nfull:\n%s", want, joined)
		}
	}
}

// no -od work = nil frame. non-od runs see the same TUI as before
func TestRenderODFrameInactiveYieldsNil(t *testing.T) {
	if got := renderODFrame(nil, 0, 86); got != nil {
		t.Errorf("nil odMetrics: want nil, got %v", got)
	}
	idle := &ulpengine.ODMetrics{}
	if got := renderODFrame(idle, 0, 86); got != nil {
		t.Errorf("phaseIdle: want nil, got %v", got)
	}
	done := &ulpengine.ODMetrics{}
	done.Phase.Store(int32(ulpengine.ODPhaseDone))
	if got := renderODFrame(done, 0, 86); got != nil {
		t.Errorf("phaseDone: want nil, got %v", got)
	}
}

// regen phase: archive count, X/Y, bytes denom. substring-level
// checks survive border-rune fiddling
func TestRenderODFrameRegenContents(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(12)
	m.ArchivesNeedRegen.Store(3)
	m.ArchivesRegenedDone.Store(1)
	m.RegenBytesTotal.Store(187 * 1024 * 1024 * 1024) // 187 GB
	m.RegenBytesRead.Store(62 * 1024 * 1024 * 1024)
	m.KeysTotalEstimate.Store(1_500_000_000)
	m.KeysLoaded.Store(500_000_000)

	lines := renderODFrame(m, 0, 86)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"Destination dedup",
		"indexing archives",
		"12 archives",
		"1 / 3 indexing",
		"62.0 GB",
		"187.0 GB",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("regen frame missing %q\nfull:\n%s", want, joined)
		}
	}
}

func TestRenderODFrameUpgradeContents(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseUpgrade))
	m.ArchivesTotal.Store(38)
	m.FilesTotal.Store(66)
	m.PartsRegenTotal.Store(56)
	m.PartsRegenDone.Store(39)

	lines := renderODFrame(m, 0, 86)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"Destination dedup",
		"upgrading index format",
		"One-time library upgrade",
		"do not interrupt",
		"your .zst archives are safe",
		"in-place re-sort",
		"38 archives",
		"39 / 56 parts indexed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("upgrade frame missing %q\nfull:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "indexing archives + writing .idx") {
		t.Errorf("upgrade frame must not use regen label\nfull:\n%s", joined)
	}
}

// active odMetrics: OD frame stacks between bars and footer
func TestRenderShardLinesIncludesODFrame(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	r.OdMetrics = &ulpengine.ODMetrics{}
	r.OdMetrics.Phase.Store(int32(ulpengine.ODPhaseRegen))
	r.OdMetrics.ArchivesTotal.Store(5)

	lines := renderShardLines(time.Now(), time.Second, &ulpengine.Metrics{}, r, 100, 100, 1, 1, 0, 86)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Destination dedup") {
		t.Errorf("shard render missing OD frame\nfull:\n%s", joined)
	}
}

// -od off = zero TUI impact, not even spacer blank line
func TestRenderShardLinesNoODWhenInactive(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	lines := renderShardLines(time.Now(), time.Second, &ulpengine.Metrics{}, r, 100, 100, 1, 1, 0, 86)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "Destination dedup") {
		t.Errorf("non-od run leaked OD frame into TUI\nfull:\n%s", joined)
	}
}

// OD library recap must follow main COMPLETE stats frame
func TestRenderFinalStdoutSummaryODBlockOrder(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	r.Cfg.DestDedup = true
	r.OdResult = &ulpengine.ODResult{
		ArchivesTotal:   5,
		ArchivesFresh:   3,
		ArchivesRegen:   2,
		TotalKeysLoaded: 1_000_000,
		Elapsed:         30 * time.Second,
	}
	r.OutputPaths = []string{filepath.Join(t.TempDir(), "sfu_xyz.txt.zst")}

	out := renderFinalStdoutSummary(60*time.Second, &ulpengine.Metrics{}, r, 86, nil)
	joined := strings.Join(out, "\n")

	odIdx := strings.Index(joined, "lines in library")
	doneIdx := strings.Index(joined, "COMPLETE")
	if odIdx < 0 || doneIdx < 0 {
		t.Fatalf("missing sections; od=%d done=%d\nfull:\n%s", odIdx, doneIdx, joined)
	}
	if odIdx <= doneIdx {
		t.Errorf("library recap should follow COMPLETE; odIdx=%d doneIdx=%d", odIdx, doneIdx)
	}

	// no "Sidecar" path row, user maintains archives not index files
	if strings.Contains(joined, "Sidecar") {
		t.Errorf("post-run summary should NOT mention sidecar path (kept internal)\nfull:\n%s", joined)
	}

	// plural: "1 archive" not "1 archives"
	if strings.Contains(joined, "1 archives") {
		t.Errorf("plural bug: '1 archives' should be '1 archive'\nfull:\n%s", joined)
	}

	if !strings.Contains(joined, "1,000,000") {
		t.Errorf("library recap should show formatted entry count\nfull:\n%s", joined)
	}
}

// library line count on its own row, never ellipsised
func TestRenderODSummaryLargeCountNotTruncated(t *testing.T) {
	r := &ulpengine.Resolved{}
	r.OdResult = &ulpengine.ODResult{
		ArchivesTotal:   3,
		TotalKeysLoaded: 15_234_567_890_123,
	}
	out := strings.Join(renderODSummary(r, nil, 86), "\n")
	want := formatCount(15_234_567_890_123)
	if !strings.Contains(out, want) {
		t.Errorf("recap missing full count %q\nout:\n%s", want, out)
	}
	if strings.Contains(out, "…") {
		t.Errorf("recap must not ellipsize library count\nout:\n%s", out)
	}
}

func TestRenderODSummaryShowsUpgradeComplete(t *testing.T) {
	r := &ulpengine.Resolved{}
	r.OdResult = &ulpengine.ODResult{
		ArchivesTotal:    2,
		TotalKeysLoaded:  1000,
		ArchivesUpgraded: 3,
	}
	out := strings.Join(renderODSummary(r, nil, 86), "\n")
	if !strings.Contains(out, "Index format upgraded") {
		t.Errorf("summary missing upgrade line\nout:\n%s", out)
	}
	if !strings.Contains(out, "3 parts") {
		t.Errorf("summary missing part count\nout:\n%s", out)
	}
}

func TestRenderODSummaryIncludesNewlyAddedLines(t *testing.T) {
	r := &ulpengine.Resolved{}
	r.OdResult = &ulpengine.ODResult{
		ArchivesTotal:   3,
		TotalKeysLoaded: 1_000_000,
	}
	m := &ulpengine.Metrics{}
	m.LinesUnique.Store(42_500)
	out := strings.Join(renderODSummary(r, m, 86), "\n")
	want := formatCount(1_042_500)
	if !strings.Contains(out, want) {
		t.Errorf("recap should include prior library + new unique lines; want %q\nout:\n%s", want, out)
	}
	if strings.Contains(out, formatCount(1_000_000)) {
		t.Errorf("recap should not show pre-run count alone when new lines were added\nout:\n%s", out)
	}
}

// COMPLETE frame always shows lines read, not just when -od is on
func TestRenderDoneLinesIncludesLinesRead(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesRead.Store(1_234_567)
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 20, InputFileCount: 2, Workers: 1,
		DedupWorkers: 1, BucketCount: 1,
	}
	out := strings.Join(renderDoneLines(time.Second, m, r, 86), "\n")
	if !strings.Contains(out, "1,234,567") {
		t.Errorf("DONE frame should show lines read\nfull:\n%s", out)
	}
	if !strings.Contains(out, "read") {
		t.Errorf("DONE frame should label lines read\nfull:\n%s", out)
	}
}

// -del paths print before COMPLETE, summary stays visible at scale
func TestRenderFinalStdoutSummaryDeletedBeforeComplete(t *testing.T) {
	r := &ulpengine.Resolved{
		DeletedInputPaths: []string{"/data/in/a.txt", "/data/in/b.txt"},
	}
	out := strings.Join(renderFinalStdoutSummary(time.Second, &ulpengine.Metrics{}, r, 86, nil), "\n")
	idxDel := strings.Index(out, "Deleted")
	idxDone := strings.Index(out, "COMPLETE")
	if idxDel < 0 || idxDone < 0 {
		t.Fatalf("expected Deleted and COMPLETE in summary\n%s", out)
	}
	if idxDel > idxDone {
		t.Fatalf("Deleted block should appear before COMPLETE\ndeleted@%d complete@%d\n%s", idxDel, idxDone, out)
	}
}

// -del paths get same gutter as output paths
func TestRenderDoneDeletedFooterListsAllPaths(t *testing.T) {
	r := &ulpengine.Resolved{}
	r.DeletedInputPaths = []string{
		"/data/in/a.txt",
		"/data/in/b.txt",
		"/data/in/c.txt",
	}
	out := strings.Join(renderDoneDeletedFooter(r), "\n")
	for _, p := range r.DeletedInputPaths {
		if !strings.Contains(out, p) {
			t.Errorf("deleted footer missing %q\nfull:\n%s", p, out)
		}
	}
	if !strings.Contains(out, "Deleted") {
		t.Errorf("deleted footer missing label\nfull:\n%s", out)
	}
}

// skipped archive paths must appear in final summary
func TestRenderODSkippedPathsListed(t *testing.T) {
	r := &ulpengine.Resolved{TotalInputs: 1 << 20, InputFileCount: 1, Workers: 1, DedupWorkers: 1, BucketCount: 1}
	r.Cfg.DestDedup = true
	r.OdResult = &ulpengine.ODResult{
		ArchivesTotal:       3,
		ArchivesSkipped:     2,
		SkippedArchivePaths: []string{"/lib/sfu_a.txt.zst", "/lib/sfu_b.txt.zst"},
	}
	out := strings.Join(renderFinalStdoutSummary(time.Second, &ulpengine.Metrics{}, r, 86, nil), "\n")
	for _, p := range r.OdResult.SkippedArchivePaths {
		if !strings.Contains(out, p) {
			t.Errorf("skipped archive path %q must appear in summary; got:\n%s", p, out)
		}
	}
}

// >5 skipped = truncate w/ "and N more" trailer
func TestRenderODSkippedPathsTruncated(t *testing.T) {
	paths := []string{}
	for i := 0; i < 12; i++ {
		paths = append(paths, fmt.Sprintf("/lib/sfu_%02d.txt.zst", i))
	}
	r := &ulpengine.Resolved{TotalInputs: 1 << 20, InputFileCount: 1, Workers: 1, DedupWorkers: 1, BucketCount: 1}
	r.Cfg.DestDedup = true
	r.OdResult = &ulpengine.ODResult{
		ArchivesTotal:       12,
		ArchivesSkipped:     12,
		SkippedArchivePaths: paths,
	}
	out := strings.Join(renderODSkippedPaths(r, 86), "\n")
	if !strings.Contains(out, "and 7 more") {
		t.Errorf("expected 'and 7 more' trailer; got:\n%s", out)
	}
}

// -od skipped lines = DONE Removed row gets "already in library" bullet
func TestRenderDoneLinesIncludesLibrarySkipped(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesAccepted.Store(100)
	m.LinesUnique.Store(80)
	m.LinesRejected.Store(5)
	m.LinesSkippedByDest.Store(15)
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 20, InputFileCount: 1, Workers: 1,
		DedupWorkers: 1, BucketCount: 1,
	}
	out := renderDoneLines(time.Second, m, r, 86)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "already in library") {
		t.Errorf("DONE row should include 'already in library' when linesSkippedByDest > 0\nfull:\n%s", joined)
	}
	if !strings.Contains(joined, "15") {
		t.Errorf("DONE row should show 15 as the dest-skipped count\nfull:\n%s", joined)
	}
}

// -odr dry-run: the live header carries a DRY RUN badge, the COMPLETE frame is
// relabeled, an explicit "nothing written" banner sits inside the box, the
// output footer states the dry-run, and the library recap reports the
// unchanged pre-run total (not total + would-be-added).
func TestRenderDryRunSummary(t *testing.T) {
	m := &ulpengine.Metrics{}
	m.LinesAccepted.Store(100)
	m.LinesUnique.Store(7) // would-be-added; must NOT inflate the library total
	m.LinesRejected.Store(3)
	m.LinesSkippedByDest.Store(20)
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 20, InputFileCount: 1, Workers: 1,
		DedupWorkers: 1, BucketCount: 1,
	}
	r.Cfg.DestDedup = true
	r.Cfg.Compress = true
	r.Cfg.DryRun = true
	r.OdResult = &ulpengine.ODResult{
		ArchivesTotal:   4,
		TotalKeysLoaded: 1_000_000,
	}

	// live header badge
	if badges := renderDedupHeaderBadges(r); !strings.Contains(badges, "DRY RUN") {
		t.Errorf("dry-run header missing DRY RUN badge; got %q", badges)
	}

	out := renderFinalStdoutSummary(time.Second, m, r, 100, nil)
	joined := strings.Join(out, "\n")
	for _, want := range []string{
		"COMPLETE · DRY RUN",
		"DRY RUN — nothing written to the library",
		"(dry run — nothing written)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dry-run summary missing %q\nfull:\n%s", want, joined)
		}
	}
	// library recap = unchanged pre-run total (1,000,000), not 1,000,007
	if strings.Contains(joined, "1,000,007") {
		t.Errorf("dry-run library total must stay unchanged; got 1,000,007\nfull:\n%s", joined)
	}
	if !strings.Contains(joined, "1,000,000") {
		t.Errorf("dry-run library total should show unchanged 1,000,000\nfull:\n%s", joined)
	}
}
