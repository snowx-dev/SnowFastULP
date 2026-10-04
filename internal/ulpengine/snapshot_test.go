package ulpengine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
)

func nowAt(sec int) time.Time { return time.Date(2026, 9, 22, 12, 0, sec, 0, time.UTC) }

// minimalResolved gives a Resolved the config-derived fields the snapshot
// builder reads.
func minimalResolved(totalInputs int64) *Resolved {
	return &Resolved{
		TotalInputs:  totalInputs,
		Workers:      4,
		DedupWorkers: 3,
	}
}

func TestStatsSnapshotParsingPhase(t *testing.T) {
	m := &Metrics{TotalInputBytes: 1000}
	m.Phase.Store(phaseShard)
	m.BytesRead.Store(400)
	m.BytesShard.Store(200)
	m.LinesRead.Store(90)
	m.LinesAccepted.Store(85)
	m.LinesRejected.Store(5)
	m.ChunksDone.Store(2)
	m.ChunksTotal.Store(8)
	m.BusyWorkers.Store(3)

	r := minimalResolved(1000)
	s := StatsSnapshot(m, r, tuistat.Rates{Read: 12.5, Shard: 6}, nowAt(1))
	if s.Phase != tuistat.PhaseParsing {
		t.Fatalf("phase = %q, want parsing", s.Phase)
	}
	if s.Fraction != 0.4 {
		t.Fatalf("fraction = %v, want 0.4", s.Fraction)
	}
	if s.Bytes == nil || s.Bytes.Read != 400 || s.Bytes.Total != 1000 || s.Bytes.BPS != 12.5 || s.Bytes.Shard != 200 {
		t.Fatalf("bytes block = %+v", s.Bytes)
	}
	if s.Lines == nil || s.Lines.Read != 90 || s.Lines.Accepted != 85 || s.Lines.Rejected != 5 {
		t.Fatalf("lines block = %+v", s.Lines)
	}
	if s.Chunks == nil || s.Chunks.Done != 2 || s.Chunks.Total != 8 {
		t.Fatalf("chunks block = %+v", s.Chunks)
	}
	if s.Workers == nil || s.Workers.Busy != 3 || s.Workers.Total != 4 {
		t.Fatalf("workers block = %+v", s.Workers)
	}
	// PhaseInit renders the same PARSING panel as PhaseShard.
	m.Phase.Store(phaseInit)
	if got := StatsSnapshot(m, r, tuistat.Rates{}, nowAt(2)).Phase; got != tuistat.PhaseParsing {
		t.Fatalf("phaseInit mapped to %q, want parsing", got)
	}
}

func TestStatsSnapshotPreparingLibrary(t *testing.T) {
	m := &Metrics{TotalInputBytes: 1000}
	m.Phase.Store(phasePhase0)
	r := minimalResolved(1000)
	r.OdMetrics = &ODMetrics{}
	od := r.OdMetrics
	od.Phase.Store(int32(odPhaseRegen))
	od.ArchivesTotal.Store(6)
	od.FilesTotal.Store(14)
	od.PartsRegenDone.Store(3)
	od.PartsRegenTotal.Store(9)
	od.RegenBytesRead.Store(300)
	od.RegenBytesTotal.Store(600)
	od.KeysTotalEstimate.Store(1500)

	s := StatsSnapshot(m, r, tuistat.Rates{Regen: 99}, nowAt(3))
	if s.Phase != tuistat.PhasePreparingLibrary {
		t.Fatalf("phase = %q, want preparing-library", s.Phase)
	}
	// Parts regen is the finer of the two progress pairs and owns the bar.
	if s.Fraction != 3.0/9.0 {
		t.Fatalf("fraction = %v, want parts ratio 1/3", s.Fraction)
	}
	if s.Library == nil || s.Library.Archives != 6 || s.Library.FilesTotal != 14 ||
		s.Library.PartsRegenDone != 3 || s.Library.PartsRegenTotal != 9 ||
		s.Library.RegenBytesDone != 300 || s.Library.RegenBytesTotal != 600 ||
		s.Library.KeysEstimate != 1500 || s.Library.RegenBPS != 99 {
		t.Fatalf("library block = %+v", s.Library)
	}
}

func TestStatsSnapshotUpgradingLibrary(t *testing.T) {
	m := &Metrics{TotalInputBytes: 1000}
	m.Phase.Store(phasePhase0)
	r := minimalResolved(1000)
	r.OdMetrics = &ODMetrics{}
	r.OdMetrics.Phase.Store(int32(odPhaseUpgrade))
	s := StatsSnapshot(m, r, tuistat.Rates{}, nowAt(4))
	if s.Phase != tuistat.PhaseUpgradingLibrary {
		t.Fatalf("phase = %q, want upgrading-library", s.Phase)
	}
}

func TestStatsSnapshotDedupPhase(t *testing.T) {
	m := &Metrics{TotalInputBytes: 1000}
	m.Phase.Store(phaseDedup)
	m.BucketsDone.Store(2)
	m.BucketsTotal.Store(5)
	m.BucketsBytesRead.Store(300)
	m.BucketsBytesTotal.Store(900)
	m.BytesWritten.Store(500)
	m.LinesUnique.Store(40)
	m.LinesRejected.Store(1)
	m.LinesSkippedByDest.Store(9)
	m.BusyWorkers.Store(2)

	r := minimalResolved(1000)
	r.DedupWorkers = 3
	r.OdMetrics = &ODMetrics{}
	r.OdMetrics.KeysTotalEstimate.Store(1000)
	r.OdMetrics.KeysLoaded.Store(500)

	s := StatsSnapshot(m, r, tuistat.Rates{Write: 77}, nowAt(5))
	if s.Phase != tuistat.PhaseDeduping {
		t.Fatalf("phase = %q, want deduping", s.Phase)
	}
	// Byte-level bucket progress owns the bar while it is known.
	if s.Fraction != 1.0/3.0 {
		t.Fatalf("fraction = %v, want 1/3", s.Fraction)
	}
	if s.Buckets == nil || s.Buckets.Done != 2 || s.Buckets.Total != 5 ||
		s.Buckets.BytesDone != 300 || s.Buckets.BytesTotal != 900 {
		t.Fatalf("buckets block = %+v", s.Buckets)
	}
	if s.Lines == nil || s.Lines.Unique != 40 || s.Lines.InLibrary != 9 {
		t.Fatalf("lines block = %+v", s.Lines)
	}
	// Write throughput rides the same bytes block, mirroring the TUI dedup
	// panel.
	if s.Bytes == nil || s.Bytes.Written == 0 || s.Bytes.BPS != 77 {
		t.Fatalf("bytes block = %+v, want written bytes and write bps 77", s.Bytes)
	}
	// The bucket bar serializes as bytes_read per the wire contract.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	bk, _ := wire["buckets"].(map[string]any)
	if _, ok := bk["bytes_read"]; !ok {
		t.Fatalf("buckets wire key drift, want bytes_read: %v", bk)
	}
	if s.Workers == nil || s.Workers.Busy != 2 || s.Workers.Total != 3 {
		t.Fatalf("workers block = %+v, want busy=2 total=dedup(3)", s.Workers)
	}
	if s.Library == nil || s.Library.KeysEstimate != 1000 || s.Library.KeysLoaded != 500 {
		t.Fatalf("library keys = %+v", s.Library)
	}
}

func TestStatsSnapshotDonePhase(t *testing.T) {
	m := &Metrics{TotalInputBytes: 100}
	m.Phase.Store(phaseDone)
	m.LinesUnique.Store(10)
	m.LinesAccepted.Store(12)
	m.LinesRejected.Store(1)
	m.LinesSkippedByDest.Store(1)

	r := minimalResolved(100)
	r.OutputPaths = []string{"/out/sfu_x.txt.zst"}
	s := StatsSnapshot(m, r, tuistat.Rates{}, nowAt(6))
	if s.Phase != tuistat.PhaseDone {
		t.Fatalf("phase = %q, want done", s.Phase)
	}
	if s.Fraction != 1 {
		t.Fatalf("fraction = %v, want 1", s.Fraction)
	}
	if s.Output == nil || len(s.Output.Paths) != 1 || s.Output.Paths[0] != "/out/sfu_x.txt.zst" {
		t.Fatalf("output block = %+v", s.Output)
	}
}

// Dry runs write nothing, so the snapshot must not surface scratch output paths.
func TestStatsSnapshotDryRunOmitsOutput(t *testing.T) {
	m := &Metrics{TotalInputBytes: 100}
	m.Phase.Store(phaseDone)
	r := minimalResolved(100)
	r.Cfg.DryRun = true
	r.OutputPaths = []string{"/tmp/sfu-odr-x/out"}
	s := StatsSnapshot(m, r, tuistat.Rates{}, nowAt(7))
	if !s.DryRun {
		t.Fatal("dry_run flag lost")
	}
	if s.Output != nil {
		t.Fatalf("dry run must omit output paths: %+v", s.Output)
	}
}

func TestStatsSnapshotHistoryRecap(t *testing.T) {
	m := &Metrics{TotalInputBytes: 100}
	m.Phase.Store(phaseShard)
	r := minimalResolved(100)
	r.HistoryChecked = 5
	r.HistorySkipped = 2
	s := StatsSnapshot(m, r, tuistat.Rates{}, nowAt(8))
	if s.History == nil || s.History.Checked != 5 || s.History.Skipped != 2 || !s.History.Enabled {
		t.Fatalf("history block = %+v", s.History)
	}
}

func TestStatsSnapshotODRegenWorkerRows(t *testing.T) {
	m := &Metrics{TotalInputBytes: 100}
	m.Phase.Store(phasePhase0)
	r := minimalResolved(100)
	r.OdMetrics = &ODMetrics{}
	r.OdMetrics.Phase.Store(int32(odPhaseRegen))
	r.OdMetrics.setWorkerSlots(2)
	name := "old.zip"
	ws0 := &r.OdMetrics.Workers[0]
	ws0.ArchivePath.Store(&name)
	ws0.PartIdx.Store(1)
	ws0.PartsTotal.Store(4)
	ws0.BytesDone.Store(50)
	ws0.BytesTotal.Store(100)

	s := StatsSnapshot(m, r, tuistat.Rates{}, nowAt(9))
	if s.Workers == nil || len(s.Workers.Active) != 1 {
		t.Fatalf("worker rows = %+v, want exactly the one busy slot", s.Workers)
	}
	row := s.Workers.Active[0]
	if row.Path != "old.zip" || row.PartIdx != 1 || row.PartsTotal != 4 ||
		row.BytesDone != 50 || row.BytesTotal != 100 {
		t.Fatalf("worker row = %+v", row)
	}
}

// No -od: the library block must be absent from every phase.
func TestStatsSnapshotWithoutODOmitsLibrary(t *testing.T) {
	m := &Metrics{TotalInputBytes: 100}
	m.Phase.Store(phaseDedup)
	s := StatsSnapshot(m, minimalResolved(100), tuistat.Rates{}, nowAt(10))
	if s.Library != nil {
		t.Fatalf("library block without -od: %+v", s.Library)
	}
}
