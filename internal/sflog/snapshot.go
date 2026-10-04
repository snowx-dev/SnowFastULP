package sflog

import (
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
)

// TUIStatSnapshot renders the tracker's current state as a tuistat.Snapshot
// for the -json stream — the JSON twin of the live TUI frame. Safe to
// call from any goroutine; during an ingest it reads the same view provider
// the TUI polls (see BeginIngest), so that provider must tolerate a second
// concurrent caller (sfl guards it with a mutex).
func (p *Progress) TUIStatSnapshot(byteRate float64, now time.Time) tuistat.Snapshot {
	s := tuistat.Snapshot{
		DryRun:   p.DryRun(),
		Fraction: p.Fraction(),
	}
	switch p.Phase() {
	case phaseDiscover:
		s.Phase = tuistat.PhaseScanning
		s.Sources = &tuistat.SourcesBlock{Discovered: p.Discovered()}
		return s
	case phaseHistory:
		s.Phase = tuistat.PhaseCheckingHistory
		done, total := p.HistoryBytesDone(), p.HistoryBytesTotal()
		s.History = &tuistat.HistoryBlock{
			Enabled:    true,
			BytesDone:  done,
			BytesTotal: total,
		}
		// History progress rides its own counters, not the byte bar's; the
		// phase completes near-instantly under the sampled fingerprint, so
		// no rate is reported for it.
		if total > 0 {
			s.Fraction = float64(done) / float64(total)
		}
		return s
	case phaseIngest:
		snapshotIngestView(&s, p)
		return s
	default:
		// phaseExtract and phaseDone share the extraction stat set; the
		// phase name alone tells COMPLETE from EXTRACTING.
		if p.Phase() == phaseDone {
			s.Phase = tuistat.PhaseDone
		} else {
			// Same rule as the TUI: before discovery reports a total, the
			// run is still scanning.
			s.Phase = tuistat.PhaseScanning
			if p.Total() > 0 {
				s.Phase = tuistat.PhaseExtracting
			}
		}
		// Blocks that carry no data yet are left nil so a cold run does not
		// emit empty {} objects — same rule as libraryBlock below.
		if p.DoneBytes() != 0 || p.Total() != 0 || byteRate != 0 {
			s.Bytes = &tuistat.BytesBlock{
				Read:  p.DoneBytes(),
				Total: p.Total(),
				BPS:   byteRate,
			}
		}
		if p.Emitted() != 0 || p.Duplicates() != 0 {
			s.Lines = &tuistat.LinesBlock{
				Unique: p.Emitted(),
				Dupes:  p.Duplicates(),
			}
		}
		if p.Files() != 0 || p.Archives() != 0 || p.Logs() != 0 ||
			p.LogsTotal() != 0 || p.Discovered() != 0 {
			s.Sources = &tuistat.SourcesBlock{
				Files:      p.Files(),
				Archives:   p.Archives(),
				LogsDone:   p.Logs(),
				LogsTotal:  p.LogsTotal(),
				Discovered: p.Discovered(),
			}
		}
		if p.EnvEnabled() {
			s.Env = &tuistat.EnvBlock{
				Enabled: true,
				Copied:  p.EnvCopied(),
				Deduped: p.EnvDeduped(),
			}
		}
		// Extraction worker rows mirror the TUI's worker panel: busy slots
		// first, capped so a large -workers run does not flood the stream.
		// Idle slots are absent, so a finished run's terminal frame omits the
		// block entirely.
		if total := p.WorkerCount(); total > 0 {
			if active := p.ActiveWorkers(total); len(active) > 0 {
				rows := make([]tuistat.WorkerRow, 0, min(len(active), MaxWorkerRows))
				for _, w := range active[:min(len(active), MaxWorkerRows)] {
					rows = append(rows, tuistat.WorkerRow{
						Path:  w.Path,
						Stage: w.Stage.String(),
					})
				}
				s.Workers = &tuistat.WorkersBlock{
					Busy:   int32(len(active)),
					Total:  int32(total),
					Active: rows,
				}
			}
		}
		return s
	}
}

// snapshotIngestView maps an in-flight library ingest view onto the shared
// snapshot blocks: the ULP read, the bucket merge, the OD regen library row,
// and the per-archive regen workers.
func snapshotIngestView(s *tuistat.Snapshot, p *Progress) {
	iv, ok := p.IngestSnapshot()
	if !ok {
		return
	}
	s.Phase = tuistat.PhaseIngesting
	s.Status = iv.Status
	s.Fraction = iv.Fraction
	s.Bytes = &tuistat.BytesBlock{
		Read:  iv.BytesRead,
		Total: iv.ULPBytes,
	}
	s.Lines = &tuistat.LinesBlock{
		Read:      iv.LinesRead,
		Unique:    iv.Unique,
		InLibrary: iv.Skipped,
	}
	s.Buckets = &tuistat.BucketsBlock{
		DoneTotal:  tuistat.DoneTotal{Done: iv.BucketsDone, Total: iv.BucketsTotal},
		BytesDone:  iv.BucketsBytesRead,
		BytesTotal: iv.BucketsBytesTotal,
	}
	s.Library = libraryBlock(iv)
	if len(iv.Workers) > 0 {
		// The ingest provider sizes its rows by terminal height, but the
		// JSON stream must not depend on the terminal: cap like the
		// extraction branch.
		n := min(len(iv.Workers), MaxWorkerRows)
		rows := make([]tuistat.WorkerRow, 0, n)
		for _, w := range iv.Workers[:n] {
			rows = append(rows, tuistat.WorkerRow{
				Path:       w.Archive,
				PartIdx:    w.PartIdx,
				PartsTotal: w.PartsTotal,
				BytesDone:  w.BytesDone,
				BytesTotal: w.BytesTotal,
			})
		}
		s.Workers = &tuistat.WorkersBlock{Active: rows}
	}
}

// libraryBlock maps an ingest view's destination-library rows onto the shared
// library block. It returns nil while every library field is still zero so a
// cold ingest (e.g. -od against an empty destination) never emits an empty
// "library":{} object.
func libraryBlock(iv IngestView) *tuistat.LibraryBlock {
	if iv.LibraryKeys == 0 && iv.KeysLoaded == 0 && iv.ArchivesTotal == 0 &&
		iv.FilesTotal == 0 && iv.PartsRegenDone == 0 && iv.PartsRegenTotal == 0 &&
		iv.PartsUpgradeTotal == 0 &&
		iv.RegenBytesRead == 0 && iv.RegenBytesTotal == 0 && iv.RegenBPS == 0 {
		return nil
	}
	return &tuistat.LibraryBlock{
		KeysEstimate:      iv.LibraryKeys,
		KeysLoaded:        iv.KeysLoaded,
		Archives:          iv.ArchivesTotal,
		FilesTotal:        iv.FilesTotal,
		PartsRegenDone:    iv.PartsRegenDone,
		PartsRegenTotal:   iv.PartsRegenTotal,
		PartsUpgradeTotal: iv.PartsUpgradeTotal,
		RegenBytesDone:    iv.RegenBytesRead,
		RegenBytesTotal:   iv.RegenBytesTotal,
		RegenBPS:          iv.RegenBPS,
	}
}

// MaxWorkerRows bounds the worker rows (extraction and ingest) in an sfl
// snapshot, matching the other panels' caps so a 64-worker run doesn't flood
// the stream and JSON output stays terminal-independent.
const MaxWorkerRows = 16
