package ulpengine

import (
	"math"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
)

// StatsSnapshot renders the dedup engine's current state as a tuistat.Snapshot
// for the -json stream — the JSON twin of sfu's live TUI frames. It reads
// the same atomic counters the monitor polls and is safe to call from any
// goroutine. rates are precomputed throughputs (see tuistat.RateSampler);
// each phase only surfaces the rates that belong to it, mirroring the TUI.
func StatsSnapshot(m *Metrics, r *Resolved, rates tuistat.Rates, now time.Time) tuistat.Snapshot {
	s := tuistat.Snapshot{DryRun: r.Cfg.DryRun}
	switch m.Phase.Load() {
	case phaseInit, phaseShard:
		s.Phase = tuistat.PhaseParsing
		s.Fraction = clamp01(ratio(m.BytesRead.Load(), r.TotalInputs))
		s.Bytes = &tuistat.BytesBlock{
			Read:  m.BytesRead.Load(),
			Total: r.TotalInputs,
			BPS:   rates.Read,
			Shard: m.BytesShard.Load(),
		}
		s.Lines = linesBlock(m)
		if c := m.ChunksTotal.Load(); c != 0 {
			s.Chunks = &tuistat.DoneTotal{Done: m.ChunksDone.Load(), Total: c}
		}
		s.Workers = &tuistat.WorkersBlock{Busy: m.BusyWorkers.Load(), Total: int32(r.Workers)}
	case phasePhase0:
		// The OD frame is primary here: discovery, sidecar regen and the
		// in-place sidecar upgrade, same labels the TUI shows.
		odPhaseBlock(r.OdMetrics, &s, rates)
		if s.Phase == "" {
			s.Phase = tuistat.PhasePreparingLibrary
		}
	case phaseDedup:
		s.Phase = tuistat.PhaseDeduping
		s.Fraction = clamp01(dedupFraction(m))
		// bps mirrors the TUI dedup panel: write throughput while the dedup
		// bar advances.
		if w, bps := m.BytesWritten.Load(), rates.Write; w != 0 || bps != 0 {
			s.Bytes = &tuistat.BytesBlock{Written: w, BPS: bps}
		}
		s.Lines = linesBlock(m)
		s.Buckets = &tuistat.BucketsBlock{
			DoneTotal:  tuistat.DoneTotal{Done: m.BucketsDone.Load(), Total: m.BucketsTotal.Load()},
			BytesDone:  m.BucketsBytesRead.Load(),
			BytesTotal: m.BucketsBytesTotal.Load(),
		}
		s.Workers = &tuistat.WorkersBlock{Busy: m.BusyWorkers.Load(), Total: int32(r.DedupWorkers)}
		odLibrary(r.OdMetrics, &s)
	case phaseDone:
		s.Phase = tuistat.PhaseDone
		s.Fraction = 1
		s.Lines = linesBlock(m)
		if rd, wr := m.BytesRead.Load(), m.BytesWritten.Load(); rd != 0 || wr != 0 {
			s.Bytes = &tuistat.BytesBlock{Read: rd, Written: wr}
		}
		odLibrary(r.OdMetrics, &s)
		s.Output = outputBlock(r)
	default:
		s.Phase = tuistat.PhaseParsing
	}
	s.History = historyBlock(r)
	return s
}

// linesBlock is the shared Lines read/accepted/rejected row, plus the dest
// counters the -od phases know.
func linesBlock(m *Metrics) *tuistat.LinesBlock {
	return &tuistat.LinesBlock{
		Read:            m.LinesRead.Load(),
		Accepted:        m.LinesAccepted.Load(),
		Rejected:        m.LinesRejected.Load(),
		Unique:          m.LinesUnique.Load(),
		InLibrary:       m.LinesSkippedByDest.Load(),
		Unrepresentable: m.LinesUnrepresentable.Load(),
	}
}

// odPhaseBlock renders the phase-0 state: regen progress (preparing-library) or
// the one-time sidecar upgrade (upgrading-library), with the OD worker rows.
func odPhaseBlock(od *ODMetrics, s *tuistat.Snapshot, rates tuistat.Rates) {
	if od == nil {
		return
	}
	switch ODPhase(od.Phase.Load()) {
	case odPhaseUpgrade:
		s.Phase = tuistat.PhaseUpgradingLibrary
	case odPhaseDiscover, odPhaseRegen:
		s.Phase = tuistat.PhasePreparingLibrary
	default:
		return
	}
	s.Library = &tuistat.LibraryBlock{
		KeysEstimate:      od.KeysTotalEstimate.Load(),
		Archives:          od.ArchivesTotal.Load(),
		FilesTotal:        od.FilesTotal.Load(),
		PartsRegenDone:    od.PartsRegenDone.Load(),
		PartsRegenTotal:   od.PartsRegenTotal.Load(),
		PartsUpgradeTotal: od.PartsUpgradeTotal.Load(),
		RegenBytesDone:    od.RegenBytesRead.Load(),
		RegenBytesTotal:   od.RegenBytesTotal.Load(),
		RegenBPS:          rates.Regen,
	}
	// Parts regen is the finer progress pair and owns the bar (mirrors the
	// TUI's phase-0 fraction math); fall back to regen bytes.
	if od.PartsRegenTotal.Load() > 0 {
		s.Fraction = clamp01(ratio(int64(od.PartsRegenDone.Load()), int64(od.PartsRegenTotal.Load())))
	} else {
		s.Fraction = clamp01(ratio(od.RegenBytesRead.Load(), od.RegenBytesTotal.Load()))
	}
	if rows := odWorkerRows(od); len(rows) > 0 {
		s.Workers = &tuistat.WorkersBlock{Active: rows}
	}
}

// odLibrary fills the library keys block during dedup: the live dest-key
// scan while buckets load their dest sets, and the final key count on done.
func odLibrary(od *ODMetrics, s *tuistat.Snapshot) {
	if od == nil {
		return
	}
	total := od.KeysTotalEstimate.Load()
	if total == 0 {
		return
	}
	if s.Library == nil {
		s.Library = &tuistat.LibraryBlock{}
	}
	s.Library.KeysEstimate = total
	s.Library.KeysLoaded = od.KeysLoaded.Load()
}

// odWorkerRows snapshots the non-idle OD regen/index workers, lowest slot
// first, capped like the TUI panels.
func odWorkerRows(od *ODMetrics) []tuistat.WorkerRow {
	active := od.ActiveWorkers(maxODWorkerRows)
	if len(active) == 0 {
		return nil
	}
	rows := make([]tuistat.WorkerRow, 0, len(active))
	for _, ws := range active {
		namePtr := ws.ArchivePath.Load()
		if namePtr == nil {
			continue
		}
		rows = append(rows, tuistat.WorkerRow{
			Path:       *namePtr,
			PartIdx:    ws.PartIdx.Load(),
			PartsTotal: ws.PartsTotal.Load(),
			BytesDone:  ws.BytesDone.Load(),
			BytesTotal: ws.BytesTotal.Load(),
		})
	}
	return rows
}

func historyBlock(r *Resolved) *tuistat.HistoryBlock {
	if r.HistoryChecked == 0 && r.HistorySkipped == 0 {
		return nil
	}
	return &tuistat.HistoryBlock{
		Enabled: true,
		Checked: int64(r.HistoryChecked),
		Skipped: int64(r.HistorySkipped),
	}
}

// outputBlock reports committed output paths; a dry run writes nothing, so
// it never surfaces its scratch paths.
func outputBlock(r *Resolved) *tuistat.OutputBlock {
	if r.Cfg.DryRun {
		return nil
	}
	if len(r.OutputPaths) == 0 {
		return nil
	}
	return &tuistat.OutputBlock{Paths: r.OutputPaths}
}

// dedupFraction prefers byte-level bucket progress so the fraction moves
// smoothly inside a bucket, falling back to whole-bucket completions — the
// same math the TUI's dedup bar uses.
func dedupFraction(m *Metrics) float64 {
	if total := m.BucketsBytesTotal.Load(); total > 0 {
		return ratio(m.BucketsBytesRead.Load(), total)
	}
	return ratio(m.BucketsDone.Load(), m.BucketsTotal.Load())
}

func ratio(cur, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(cur) / float64(total)
}

func clamp01(f float64) float64 {
	// NaN/Inf comparisons fall through the < and > guards below, and a NaN
	// fraction would fail json.Marshal — silently erasing the line, terminal
	// event included. Pin non-finite values to 0.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// maxODWorkerRows bounds the per-archive regen rows in a snapshot, matching
// the TUI panels' cap so a 64-worker run doesn't flood the stream.
const maxODWorkerRows = 16
