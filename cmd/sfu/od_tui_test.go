package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderODFrameDiscoverShowsLegacyHint(t *testing.T) {
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseDiscover))
	m.archivesTotal.Store(5)
	m.partsUpgradeTotal.Store(12)

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
	idle := &odMetrics{}
	if got := renderODFrame(idle, 0, 86); got != nil {
		t.Errorf("phaseIdle: want nil, got %v", got)
	}
	done := &odMetrics{}
	done.phase.Store(int32(odPhaseDone))
	if got := renderODFrame(done, 0, 86); got != nil {
		t.Errorf("phaseDone: want nil, got %v", got)
	}
}

// regen phase: archive count, X/Y, bytes denom. substring-level
// checks survive border-rune fiddling
func TestRenderODFrameRegenContents(t *testing.T) {
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseRegen))
	m.archivesTotal.Store(12)
	m.archivesNeedRegen.Store(3)
	m.archivesRegenedDone.Store(1)
	m.regenBytesTotal.Store(187 * 1024 * 1024 * 1024) // 187 GB
	m.regenBytesRead.Store(62 * 1024 * 1024 * 1024)
	m.keysTotalEstimate.Store(1_500_000_000)
	m.keysLoaded.Store(500_000_000)

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
	m := &odMetrics{}
	m.phase.Store(int32(odPhaseUpgrade))
	m.archivesTotal.Store(38)
	m.filesTotal.Store(66)
	m.partsRegenTotal.Store(56)
	m.partsRegenDone.Store(39)

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
	r := &resolved{
		totalInputs: 1 << 30, inputFileCount: 1, workers: 4,
		dedupWorkers: 2, bucketCount: 64,
	}
	r.odMetrics = &odMetrics{}
	r.odMetrics.phase.Store(int32(odPhaseRegen))
	r.odMetrics.archivesTotal.Store(5)

	lines := renderShardLines(time.Now(), time.Second, &metrics{}, r, 100, 100, 1, 1, 0, 86)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Destination dedup") {
		t.Errorf("shard render missing OD frame\nfull:\n%s", joined)
	}
}

// -od off = zero TUI impact, not even spacer blank line
func TestRenderShardLinesNoODWhenInactive(t *testing.T) {
	r := &resolved{
		totalInputs: 1 << 30, inputFileCount: 1, workers: 4,
		dedupWorkers: 2, bucketCount: 64,
	}
	lines := renderShardLines(time.Now(), time.Second, &metrics{}, r, 100, 100, 1, 1, 0, 86)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "Destination dedup") {
		t.Errorf("non-od run leaked OD frame into TUI\nfull:\n%s", joined)
	}
}

// OD library recap must follow main COMPLETE stats frame
func TestRenderFinalStdoutSummaryODBlockOrder(t *testing.T) {
	r := &resolved{
		totalInputs: 1 << 30, inputFileCount: 1, workers: 4,
		dedupWorkers: 2, bucketCount: 64,
	}
	r.cfg.DestDedup = true
	r.odResult = &odResult{
		ArchivesTotal:   5,
		ArchivesFresh:   3,
		ArchivesRegen:   2,
		TotalKeysLoaded: 1_000_000,
		Elapsed:         30 * time.Second,
	}
	r.OutputPaths = []string{filepath.Join(t.TempDir(), "sfu_xyz.txt.zst")}

	out := renderFinalStdoutSummary(60*time.Second, &metrics{}, r, 86, nil)
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
	r := &resolved{}
	r.odResult = &odResult{
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
	r := &resolved{}
	r.odResult = &odResult{
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
	r := &resolved{}
	r.odResult = &odResult{
		ArchivesTotal:   3,
		TotalKeysLoaded: 1_000_000,
	}
	m := &metrics{}
	m.linesUnique.Store(42_500)
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
	m := &metrics{}
	m.linesRead.Store(1_234_567)
	r := &resolved{
		totalInputs: 1 << 20, inputFileCount: 2, workers: 1,
		dedupWorkers: 1, bucketCount: 1,
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
	r := &resolved{
		DeletedInputPaths: []string{"/data/in/a.txt", "/data/in/b.txt"},
	}
	out := strings.Join(renderFinalStdoutSummary(time.Second, &metrics{}, r, 86, nil), "\n")
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
	r := &resolved{}
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
	r := &resolved{totalInputs: 1 << 20, inputFileCount: 1, workers: 1, dedupWorkers: 1, bucketCount: 1}
	r.cfg.DestDedup = true
	r.odResult = &odResult{
		ArchivesTotal:       3,
		ArchivesSkipped:     2,
		SkippedArchivePaths: []string{"/lib/sfu_a.txt.zst", "/lib/sfu_b.txt.zst"},
	}
	out := strings.Join(renderFinalStdoutSummary(time.Second, &metrics{}, r, 86, nil), "\n")
	for _, p := range r.odResult.SkippedArchivePaths {
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
	r := &resolved{totalInputs: 1 << 20, inputFileCount: 1, workers: 1, dedupWorkers: 1, bucketCount: 1}
	r.cfg.DestDedup = true
	r.odResult = &odResult{
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
	m := &metrics{}
	m.linesAccepted.Store(100)
	m.linesUnique.Store(80)
	m.linesRejected.Store(5)
	m.linesSkippedByDest.Store(15)
	r := &resolved{
		totalInputs: 1 << 20, inputFileCount: 1, workers: 1,
		dedupWorkers: 1, bucketCount: 1,
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
