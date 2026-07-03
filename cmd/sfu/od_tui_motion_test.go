package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// 1 archive across 16 files must show both counts, else TUI vs ls
// looks contradictory
func TestLibraryRowSurfacesFilesWhenMultipart(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.FilesTotal.Store(16)
	m.ArchivesNeedRegen.Store(1)
	m.RegenBytesTotal.Store(34 * 1024 * 1024 * 1024)
	m.RegenBytesRead.Store(10 * 1024 * 1024 * 1024)

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if !strings.Contains(out, "1 archive") {
		t.Errorf("missing archive count\nout:\n%s", out)
	}
	if !strings.Contains(out, "across 16 files") {
		t.Errorf("missing files annotation\nout:\n%s", out)
	}
}

// archives == files = drop redundant "across N files" noise
func TestLibraryRowHidesFilesWhenSingleton(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(12)
	m.FilesTotal.Store(12)
	m.ArchivesNeedRegen.Store(2)
	m.RegenBytesTotal.Store(1 << 30)
	m.RegenBytesRead.Store(1 << 28)

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if strings.Contains(out, "across") {
		t.Errorf("redundant 'across N files' suffix should be hidden\nout:\n%s", out)
	}
}

// non-zero BPS during regen = Throughput row + ETA, the "things are
// moving" signal that ticks every redraw
func TestThroughputRowShowsLiveRate(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.ArchivesNeedRegen.Store(1)
	m.RegenBytesTotal.Store(34 * 1024 * 1024 * 1024)
	m.RegenBytesRead.Store(2 * 1024 * 1024 * 1024)

	out := strings.Join(renderODFrame(m, 320e6, 100), "\n") // 320 MB/s
	if !strings.Contains(out, "Throughput") {
		t.Errorf("missing Throughput row\nout:\n%s", out)
	}
	if !strings.Contains(out, "ETA") {
		t.Errorf("missing ETA hint\nout:\n%s", out)
	}
}

// BPS legitimately samples 0 between decoder reads. row must stay,
// frame height must stay, else the phase panel jumps vertically
func TestThroughputRowStableWhenRateDropsToZero(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.ArchivesNeedRegen.Store(1)
	m.RegenBytesTotal.Store(34 * 1024 * 1024 * 1024)

	zeroRate := renderODFrame(m, 0, 100)
	liveRate := renderODFrame(m, 320e6, 100)
	out := strings.Join(zeroRate, "\n")
	if !strings.Contains(out, "Throughput") {
		t.Errorf("Throughput row should stay visible at BPS=0\nout:\n%s", out)
	}
	if strings.Contains(out, "ETA") {
		t.Errorf("ETA should be hidden at BPS=0\nout:\n%s", out)
	}
	if len(zeroRate) != len(liveRate) {
		t.Errorf("frame height changed when rate dropped to zero: zero=%d live=%d\nzero:\n%s\nlive:\n%s",
			len(zeroRate), len(liveRate), strings.Join(zeroRate, "\n"), strings.Join(liveRate, "\n"))
	}
}

// active worker renders row + mini-bar + %, idle slot (nil) doesnt
func TestWorkerRowsRenderActiveSlots(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(1)
	m.ArchivesNeedRegen.Store(1)
	m.RegenBytesTotal.Store(2 * 1024 * 1024 * 1024)
	m.RegenBytesRead.Store(1024 * 1024 * 1024)

	m.Workers = make([]ulpengine.WorkerStatus, 4)
	// slot 0: decoding part 4/16, halfway
	name := "sfu_20260514-050735_part4.txt.zst"
	m.Workers[0].ArchivePath.Store(&name)
	m.Workers[0].PartIdx.Store(4)
	m.Workers[0].PartsTotal.Store(16)
	m.Workers[0].BytesDone.Store(1 * 1024 * 1024 * 1024)
	m.Workers[0].BytesTotal.Store(2 * 1024 * 1024 * 1024)
	// slot 1: idle, must not render
	// slot 2: different archive
	name2 := "sfu_20260513-194521_part1.txt.zst"
	m.Workers[2].ArchivePath.Store(&name2)
	m.Workers[2].PartIdx.Store(1)
	m.Workers[2].PartsTotal.Store(1)
	m.Workers[2].BytesDone.Store(100 * 1024 * 1024)
	m.Workers[2].BytesTotal.Store(500 * 1024 * 1024)

	out := strings.Join(renderODFrame(m, 0, 120), "\n")
	if !strings.Contains(out, "[1]") {
		t.Errorf("active slot 0 missing [1] marker\nout:\n%s", out)
	}
	if !strings.Contains(out, "(4/16)") {
		t.Errorf("missing part annotation (4/16)\nout:\n%s", out)
	}
	if !strings.Contains(out, "20260514-050735_part4") &&
		!strings.Contains(out, "...20260514-050735_part4") &&
		!strings.Contains(out, "...0735_part4") {
		t.Errorf("missing compacted archive name for slot 0\nout:\n%s", out)
	}
	// rows indexed by active position, not slot id:
	//   slot 0 -> [1], slot 2 -> [2], so [3] must not appear
	if strings.Contains(out, "[3]") {
		t.Errorf("idle slot 1 leaked into render\nout:\n%s", out)
	}
}

// worker rows only render in regen. discovery = stale state, hide
func TestWorkerRowsHiddenOutsideRegen(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseDiscover))
	m.ArchivesTotal.Store(2)
	m.Workers = make([]ulpengine.WorkerStatus, 1)
	stale := "sfu_xyz_part1.txt.zst"
	m.Workers[0].ArchivePath.Store(&stale)
	m.Workers[0].BytesTotal.Store(1 << 30)
	m.Workers[0].BytesDone.Store(1 << 28)

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if strings.Contains(out, "[1]") {
		t.Errorf("worker row should not render outside regen phase\nout:\n%s", out)
	}
}

// cap worker rows so 16-worker runs dont dominate the frame.
// extras silently dropped, global bar still tracks them
func TestWorkerRowCapAtEight(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(16)
	m.ArchivesNeedRegen.Store(16)
	m.RegenBytesTotal.Store(1 << 40)
	m.Workers = make([]ulpengine.WorkerStatus, 16)
	for i := range m.Workers {
		s := "sfu_test_part1.txt.zst"
		m.Workers[i].ArchivePath.Store(&s)
		m.Workers[i].PartIdx.Store(1)
		m.Workers[i].PartsTotal.Store(1)
		m.Workers[i].BytesTotal.Store(1 << 30)
		m.Workers[i].BytesDone.Store(int64(i) * (1 << 27))
	}

	out := strings.Join(renderODFrame(m, 0, 200), "\n")
	for i := 1; i <= maxWorkerRowsRendered; i++ {
		if !strings.Contains(out, "["+strconv.Itoa(i)+"]") {
			t.Errorf("missing [%d] in capped render\nout:\n%s", i, out)
		}
	}
	if strings.Contains(out, "["+strconv.Itoa(maxWorkerRowsRendered+1)+"]") {
		t.Errorf("worker [%d] should be over the cap\nout:\n%s", maxWorkerRowsRendered+1, out)
	}
}

// idle slots (nil ptr) must be skipped by the helper
func TestActiveWorkersFiltersNilSlots(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Workers = make([]ulpengine.WorkerStatus, 4)
	name := "x"
	m.Workers[1].ArchivePath.Store(&name)
	m.Workers[3].ArchivePath.Store(&name)

	got := m.ActiveWorkers(10)
	if len(got) != 2 {
		t.Fatalf("activeWorkers len = %d, want 2", len(got))
	}
	if got[0] != &m.Workers[1] || got[1] != &m.Workers[3] {
		t.Errorf("activeWorkers returned wrong slots")
	}
}

func TestActiveWorkersHonorsMax(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Workers = make([]ulpengine.WorkerStatus, 10)
	for i := range m.Workers {
		s := "x"
		m.Workers[i].ArchivePath.Store(&s)
	}
	if got := m.ActiveWorkers(3); len(got) != 3 {
		t.Errorf("activeWorkers(3) len = %d, want 3", len(got))
	}
}

// post-run library frame = indexed line total only. archive metadata
// stays on the live OD frame
func TestRecapShowsLibraryLineCount(t *testing.T) {
	r := &ulpengine.Resolved{
		OdResult: &ulpengine.ODResult{
			ArchivesTotal:   1,
			FilesTotal:      16,
			TotalKeysLoaded: 12_345_678,
		},
	}
	out := strings.Join(renderODSummary(r, nil, 100), "\n")
	if !strings.Contains(out, "12,345,678") {
		t.Errorf("recap missing library line count\nout:\n%s", out)
	}
	if !strings.Contains(out, "lines in library") {
		t.Errorf("recap missing label\nout:\n%s", out)
	}
	if strings.Contains(out, "across 16 files") {
		t.Errorf("recap should not repeat live-frame archive metadata\nout:\n%s", out)
	}
}

// migration/upgrade pass has parts progress but NO byte denominator. it must
// render a single aggregate bar (parts-indexed), not many frozen per-worker
// rows. guards the v2->v3 migration UX on a first run against an old library.
func TestUpgradePassHidesWorkerRowsShowsPartsProgress(t *testing.T) {
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseUpgrade))
	m.ArchivesTotal.Store(20)
	m.PartsRegenTotal.Store(20)
	m.PartsRegenDone.Store(7)
	// regenBytesTotal stays 0 (upgrade has no byte progress)
	m.Workers = make([]ulpengine.WorkerStatus, 1)
	name := "sfu_old_part1.txt.zst"
	m.Workers[0].ArchivePath.Store(&name)
	m.Workers[0].PartIdx.Store(1)
	m.Workers[0].PartsTotal.Store(1)

	out := strings.Join(renderODFrame(m, 0, 100), "\n")
	if strings.Contains(out, "[1]") {
		t.Errorf("upgrade pass should not render frozen per-worker rows\nout:\n%s", out)
	}
	if !strings.Contains(out, "upgrading index format") {
		t.Errorf("upgrade pass should show v2->v3 label\nout:\n%s", out)
	}
	if !strings.Contains(out, "One-time library upgrade") {
		t.Errorf("upgrade pass should show one-time callout\nout:\n%s", out)
	}
	if strings.Contains(out, "indexing archives + writing .idx") {
		t.Errorf("upgrade pass must not use decompress regen label\nout:\n%s", out)
	}
	if !strings.Contains(out, "7 / 20 parts indexed") {
		t.Errorf("upgrade pass should show parts progress\nout:\n%s", out)
	}
}
