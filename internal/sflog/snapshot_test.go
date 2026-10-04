package sflog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
)

func TestProgressSnapshotScanning(t *testing.T) {
	p := NewProgress()
	p.discovered.Store(7)
	s := p.TUIStatSnapshot(0, time.Now())
	if s.Phase != tuistat.PhaseScanning {
		t.Fatalf("phase = %q, want %q", s.Phase, tuistat.PhaseScanning)
	}
	if s.Sources == nil || s.Sources.Discovered != 7 {
		t.Fatalf("sources = %+v, want discovered=7", s.Sources)
	}
	if s.Fraction != 0 {
		t.Fatalf("fraction = %v, want 0 while scanning", s.Fraction)
	}
}

func TestProgressSnapshotHistoryPhase(t *testing.T) {
	p := NewProgress()
	p.BeginHistory(100)
	p.historyDone.Store(40)
	s := p.TUIStatSnapshot(0, time.Now())
	if s.Phase != tuistat.PhaseCheckingHistory {
		t.Fatalf("phase = %q, want %q", s.Phase, tuistat.PhaseCheckingHistory)
	}
	if s.History == nil || s.History.BytesDone != 40 || s.History.BytesTotal != 100 {
		t.Fatalf("history block = %+v, want 40/100", s.History)
	}
	if !s.History.Enabled {
		t.Fatal("history block must be enabled during the history phase")
	}
	if s.Fraction != 0.4 {
		t.Fatalf("fraction = %v, want 0.4", s.Fraction)
	}
}

func TestProgressSnapshotExtractPhase(t *testing.T) {
	p := NewProgress()
	p.setPhase(phaseExtract)
	p.setTotal(1000)
	p.done.Store(400)
	p.addFile()
	p.addFile()
	p.addArchive()
	p.setLogsTotal(5)
	p.addLogDone()
	p.addEmitted()
	p.addEmitted()
	p.addDup()
	p.EnableEnv()
	p.addEnvCopied(2)
	p.addEnvDeduped()
	p.SetDryRun(true)

	s := p.TUIStatSnapshot(123.5, time.Now())
	if s.Phase != tuistat.PhaseExtracting {
		t.Fatalf("phase = %q, want %q", s.Phase, tuistat.PhaseExtracting)
	}
	if !s.DryRun {
		t.Fatal("dry_run flag lost")
	}
	if s.Fraction != 0.4 {
		t.Fatalf("fraction = %v, want 0.4", s.Fraction)
	}
	if s.Bytes == nil || s.Bytes.Read != 400 || s.Bytes.Total != 1000 || s.Bytes.BPS != 123.5 {
		t.Fatalf("bytes block = %+v, want 400/1000 @123.5", s.Bytes)
	}
	if s.Lines == nil || s.Lines.Unique != 2 || s.Lines.Dupes != 1 {
		t.Fatalf("lines block = %+v, want unique=2 dupes=1", s.Lines)
	}
	if s.Sources == nil || s.Sources.Files != 2 || s.Sources.Archives != 1 ||
		s.Sources.LogsDone != 1 || s.Sources.LogsTotal != 5 {
		t.Fatalf("sources block = %+v", s.Sources)
	}
	if s.Env == nil || !s.Env.Enabled || s.Env.Copied != 2 || s.Env.Deduped != 1 {
		t.Fatalf("env block = %+v", s.Env)
	}
}

func TestProgressSnapshotExtractWithLibraryBadge(t *testing.T) {
	p := NewProgress()
	p.SetLibrary(true)
	p.setPhase(phaseExtract)
	p.setTotal(10)
	s := p.TUIStatSnapshot(0, time.Now())
	// libraryOn alone does not change the phase; the counters stay extraction's.
	if s.Phase != tuistat.PhaseExtracting {
		t.Fatalf("phase = %q, want %q", s.Phase, tuistat.PhaseExtracting)
	}
}

func TestProgressSnapshotDonePhase(t *testing.T) {
	p := NewProgress()
	p.setTotal(100)
	p.done.Store(100)
	p.addEmitted()
	p.setPhase(phaseDone)
	s := p.TUIStatSnapshot(0, time.Now())
	if s.Phase != tuistat.PhaseDone {
		t.Fatalf("phase = %q, want %q", s.Phase, tuistat.PhaseDone)
	}
	if s.Lines == nil || s.Lines.Unique != 1 {
		t.Fatalf("final stats lost: %+v", s.Lines)
	}
}

func TestProgressSnapshotIngestPhase(t *testing.T) {
	p := NewProgress()
	p.BeginIngest(func() IngestView {
		return IngestView{
			Fraction:     0.5,
			Status:       tuistat.LibraryMerging,
			ULPBytes:     200,
			BytesRead:    100,
			LinesRead:    9,
			ShowMerge:    true,
			Unique:       3,
			Skipped:      2,
			BucketsDone:  1,
			BucketsTotal: 4,
			// Bucket/library byte counters exercise the wire contract keys
			// (bytes_read / regen_bytes_read) below.
			BucketsBytesRead: 100,
			RegenBPS:         55.5,
			RegenBytesRead:   10,
			LibraryKeys:      90,
			// Library parity with sfu's -od snapshot (N1): keys_loaded,
			// files_total, parts_upgrade_total on the ingest library block.
			KeysLoaded:        40,
			FilesTotal:        7,
			PartsUpgradeTotal: 3,
			Workers: []IngestWorker{{
				Archive: "old.zip", PartIdx: 2, PartsTotal: 5,
				BytesDone: 10, BytesTotal: 20,
			}},
		}
	})
	s := p.TUIStatSnapshot(0, time.Now())
	if s.Phase != tuistat.PhaseIngesting {
		t.Fatalf("phase = %q, want %q", s.Phase, tuistat.PhaseIngesting)
	}
	if s.Status != tuistat.LibraryMerging {
		t.Fatalf("status = %q, want %q", s.Status, tuistat.LibraryMerging)
	}
	if s.Fraction != 0.5 {
		t.Fatalf("fraction = %v, want 0.5", s.Fraction)
	}
	if s.Lines == nil || s.Lines.Unique != 3 || s.Lines.InLibrary != 2 || s.Lines.Read != 9 {
		t.Fatalf("lines block = %+v, want unique=3 in_library=2 read=9", s.Lines)
	}
	if s.Bytes == nil || s.Bytes.Read != 100 || s.Bytes.Total != 200 {
		t.Fatalf("bytes block = %+v, want 100/200", s.Bytes)
	}
	if s.Buckets == nil || s.Buckets.Done != 1 || s.Buckets.Total != 4 {
		t.Fatalf("buckets block = %+v", s.Buckets)
	}
	if s.Library == nil || s.Library.KeysEstimate != 90 || s.Library.RegenBPS != 55.5 {
		t.Fatalf("library block = %+v", s.Library)
	}
	if s.Library.KeysLoaded != 40 || s.Library.FilesTotal != 7 || s.Library.PartsUpgradeTotal != 3 {
		t.Fatalf("library parity keys = %+v, want keys_loaded=40 files_total=7 parts_upgrade_total=3", s.Library)
	}
	if s.Buckets.BytesDone != 100 {
		t.Fatalf("buckets bytes done = %d, want 100", s.Buckets.BytesDone)
	}
	if s.Library.RegenBytesDone != 10 {
		t.Fatalf("library regen bytes done = %d, want 10", s.Library.RegenBytesDone)
	}
	// Wire contract: those fields serialize under the DESIGN.md names.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	for _, wc := range []struct{ block, key string }{
		{"buckets", "bytes_read"}, {"library", "regen_bytes_read"},
		// N1 parity keys, pinned like the rest of the wire contract.
		{"library", "keys_loaded"}, {"library", "files_total"},
		{"library", "parts_upgrade_total"},
	} {
		blk, _ := wire[wc.block].(map[string]any)
		if _, ok := blk[wc.key]; !ok {
			t.Fatalf("%s wire key drift, want %q: %v", wc.block, wc.key, blk)
		}
	}
	if len(s.Workers.Active) != 1 || s.Workers.Active[0].Path != "old.zip" ||
		s.Workers.Active[0].PartIdx != 2 || s.Workers.Active[0].PartsTotal != 5 {
		t.Fatalf("worker rows = %+v", s.Workers)
	}
}

// Ingest worker rows are capped in the JSON snapshot: the ingest provider
// sizes rows by terminal height, but machine output must not depend on the
// terminal.
func TestProgressSnapshotIngestWorkerRowsCapped(t *testing.T) {
	p := NewProgress()
	p.BeginIngest(func() IngestView {
		iv := IngestView{RegenBPS: 1, LibraryKeys: 1}
		for i := int32(0); i < MaxWorkerRows+5; i++ {
			iv.Workers = append(iv.Workers, IngestWorker{
				Archive: "a.zip", PartIdx: i,
				BytesDone: int64(i), BytesTotal: 100,
			})
		}
		return iv
	})
	s := p.TUIStatSnapshot(0, time.Now())
	if s.Workers == nil {
		t.Fatal("ingest snapshot lost the workers block")
	}
	if len(s.Workers.Active) != MaxWorkerRows {
		t.Fatalf("worker rows = %d, want capped at %d", len(s.Workers.Active), MaxWorkerRows)
	}
	if s.Workers.Active[0].PartIdx != 0 || s.Workers.Active[MaxWorkerRows-1].PartIdx != int32(MaxWorkerRows-1) {
		t.Fatalf("wrong rows kept: first=%+v last=%+v", s.Workers.Active[0], s.Workers.Active[MaxWorkerRows-1])
	}
}

// No ingest is in flight: the ingest block must be entirely absent so a
// classic -o run never shows an empty ingesting section.
func TestProgressSnapshotWithoutIngestOmitsIngestBlocks(t *testing.T) {
	p := NewProgress()
	s := p.TUIStatSnapshot(0, time.Now())
	if s.Buckets != nil || s.Library != nil || s.Workers != nil {
		t.Fatalf("no-ingest snapshot must omit ingest blocks: %+v", s)
	}
}

// Extraction workers mirror the TUI panel: busy slots appear with their
// canonical stage label, idle slots are absent, and the block is omitted
// entirely once every worker finished (a done frame carries no workers).
func TestProgressSnapshotExtractionWorkers(t *testing.T) {
	p := NewProgress()
	p.SetWorkers(3)
	p.setPhase(phaseExtract)
	p.setTotal(100)
	p.setActive(0, "a.txt", StageParsing)
	p.setActive(2, "b.zip", StageExtracting) // slot 1 stays idle
	s := p.TUIStatSnapshot(0, time.Now())
	if s.Workers == nil {
		t.Fatal("busy extraction workers must reach the stream")
	}
	if s.Workers.Busy != 2 || s.Workers.Total != 3 {
		t.Fatalf("workers busy/total = %d/%d, want 2/3", s.Workers.Busy, s.Workers.Total)
	}
	if len(s.Workers.Active) != 2 {
		t.Fatalf("active rows = %+v, want the 2 busy slots", s.Workers.Active)
	}
	if s.Workers.Active[0].Path != "a.txt" || s.Workers.Active[0].Stage != "parsing" {
		t.Fatalf("row[0] = %+v, want a.txt parsing", s.Workers.Active[0])
	}
	if s.Workers.Active[1].Path != "b.zip" || s.Workers.Active[1].Stage != "extracting" {
		t.Fatalf("row[1] = %+v, want b.zip extracting", s.Workers.Active[1])
	}

	// A finished run has no busy slots, so the terminal done frame omits the
	// block instead of showing an empty list.
	for i := range p.workers {
		p.workers[i].path.Store(nil)
	}
	s = p.TUIStatSnapshot(0, time.Now())
	if s.Workers != nil {
		t.Fatalf("done frame with no busy workers must omit the block: %+v", s.Workers)
	}
}
