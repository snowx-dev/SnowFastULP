package index

import "sync/atomic"

// ArchiveByteProgress reports monotonic byte progress for one archive build/load.
type ArchiveByteProgress struct {
	done *atomic.Int64
	last int64
}

// NewArchiveByteProgress wraps done for use as a Progress callback.
func NewArchiveByteProgress(done *atomic.Int64) *ArchiveByteProgress {
	return &ArchiveByteProgress{done: done}
}

// Callback returns a Progress func for Ensure/Build/ScanFile.
func (p *ArchiveByteProgress) Callback() Progress {
	return func(bytesDone, _ int64) {
		if bytesDone < p.last {
			return
		}
		delta := bytesDone - p.last
		if delta > 0 {
			p.done.Add(delta)
			p.last = bytesDone
		}
	}
}

// Finish credits any remaining bytes for archiveSize, resets for reuse.
func (p *ArchiveByteProgress) Finish(archiveSize int64) {
	if archiveSize > p.last {
		p.done.Add(archiveSize - p.last)
	}
	p.last = 0
}
