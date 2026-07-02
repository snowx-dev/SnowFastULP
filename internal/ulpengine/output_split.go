package ulpengine

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
	"github.com/snowx-dev/SnowFastULP/internal/pathident"
)

// sfu_<stamp>_partN.txt.zst. stamp = yyyymmdd_<runID>
func zstPartPath(dir, stamp string, part int) string {
	name := fmt.Sprintf("sfu_%s_part%d.txt.zst", stamp, part)
	return filepath.Join(dir, name)
}

// write side for dedup and fast path (single file or rotating zstd)
type lineSink interface {
	writeBatch(buf []byte, lineCount int, m *Metrics) error
	close() error
	outputPaths() []string
}

// optional: -od output sinks record dedup hashes alongside archive writes
type indexedLineSink interface {
	lineSink
	writeBatchIndexed(buf []byte, hashes []uint64, lineCount int, m *Metrics) error
}

func newLineSink(r *Resolved) (lineSink, error) {
	writeSearchIdx := r.Cfg.DestDedup && r.Cfg.Compress
	indexSidecar := r.Cfg.DestDedup

	// chunked multi-part .zst output
	if r.Cfg.Compress && r.Cfg.ZstChunkLines > 0 {
		stamp := r.Cfg.RunStamp
		if stamp == "" {
			stamp = r.Cfg.RunStarted.Format("20060102")
		}
		return newChunkedZstdSink(filepath.Dir(r.Cfg.Output), stamp, r.Cfg.ZstChunkLines, r.Cfg.Debug, writeSearchIdx, indexSidecar)
	}

	// single output file (plain or single-frame .zst)
	if indexSidecar {
		return newOutputSinkWithSidecar(r.Cfg.Output, r.Cfg.Compress, writeSearchIdx)
	}
	return newOutputSink(r.Cfg.Output, r.Cfg.Compress, writeSearchIdx)
}

func sinkOutputPaths(s lineSink) []string {
	if s == nil {
		return nil
	}
	return s.outputPaths()
}

// removeOutputFiles discards a failed run's output. It removes each archive plus
// any sidecars committed for it, so a failed/cancelled run never leaves an orphan
// .idx pointing at a removed (or partial) archive — a later -od run would
// otherwise read that sidecar as authoritative.
func removeOutputFiles(paths []string) {
	for _, p := range paths {
		if p == "" {
			continue
		}
		RemovePathLogged(p)
		sc := sidecarPathForArchive(p)
		RemovePathLogged(sc)
		RemovePathLogged(searchSidecarPathForArchive(p))
	}
}

// discardEmptyOutput drops a run's generated output shard(s) and their sidecars
// when the run wrote zero unique lines. sfu and sfl always name the output
// sfu_<stamp> (the user picks a directory, never the file), so a 0-line run
// leaves nothing but a ~13-byte empty zstd frame plus a 0-key .idx — clutter
// that also makes the summary point at a file holding nothing. A completed run
// only ever leaves ONE empty shard: the chunked sink opens a new part solely to
// hold a line it is about to write, so an empty part never trails. Returns the
// surviving paths (nil once the empty shard is dropped) so callers set
// r.OutputPaths straight from it. Shared by the fast and bucketed paths so both
// tools behave identically.
func discardEmptyOutput(m *Metrics, paths []string) []string {
	if m == nil || m.LinesUnique.Load() > 0 {
		return paths
	}
	removeOutputFiles(paths)
	return nil
}

// removes collected inputs after success. skips paths matching outputs
// (after Abs+Clean and inode check). returns partial list on err
func DeleteParsedInputs(inputs, outputs []string) ([]string, error) {
	outAbs := make([]string, 0, len(outputs))
	outSet := make(map[string]struct{}, len(outputs))
	for _, p := range outputs {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		clean := filepath.Clean(a)
		outAbs = append(outAbs, clean)
		outSet[clean] = struct{}{}
	}
	var removed []string
	for _, in := range inputs {
		abs, err := filepath.Abs(in)
		if err != nil {
			return removed, err
		}
		abs = filepath.Clean(abs)
		if _, skip := outSet[abs]; skip {
			continue
		}
		// inode check vs case-folded volumes (macOS/Windows) and
		// hardlink/symlink aliases. rather skip than rm an output
		skipByIdentity := false
		for _, out := range outAbs {
			if same, err := pathident.SameFile(abs, out); err == nil && same {
				skipByIdentity = true
				break
			}
		}
		if skipByIdentity {
			continue
		}
		if err := os.Remove(abs); err != nil {
			return removed, fmt.Errorf("remove %s: %w", abs, err)
		}
		removed = append(removed, abs)
	}
	return removed, nil
}

// rotates zst parts every chunkLines unique lines. first file is
// sfu_<stamp>.txt.zst, _partN suffix only when >=2 archives open
// (part 1 produced by renaming the first file on rotate to part 2).
// dbg optional, rotation events go to debugLog.Event for timelining
type chunkedZstdSink struct {
	mu             sync.Mutex
	dir            string
	stamp          string
	chunkLines     int64
	part           int
	linesInPart    int64
	cur            *outputSink
	paths          []string
	dbg            *DebugLog
	writeSearchIdx bool
	indexSidecar   bool
}

func newChunkedZstdSink(dir, stamp string, chunkLines int64, dbg *DebugLog, writeSearchIdx, indexSidecar bool) (*chunkedZstdSink, error) {
	c := &chunkedZstdSink{dir: dir, stamp: stamp, chunkLines: chunkLines, dbg: dbg, writeSearchIdx: writeSearchIdx, indexSidecar: indexSidecar}
	if err := c.rotateLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *chunkedZstdSink) rotateLocked() error {
	prevLines := c.linesInPart
	if c.cur != nil {
		if err := c.cur.close(); err != nil {
			return err
		}
		// first file -> _part1 only when we're opening a 2nd archive
		if c.part == 1 && len(c.paths) == 1 {
			old := c.paths[0]
			newName := filepath.Clean(zstPartPath(c.dir, c.stamp, 1))
			if err := atomicfs.Rename(old, newName); err != nil {
				return fmt.Errorf("rename %s -> %s: %w", old, newName, err)
			}
			c.paths[0] = newName
			RegisterCleanupPath(newName)
			c.dbg.Event("rotate-rename: part=1 %s -> %s", old, newName)
			if c.indexSidecar {
				oldSC := sidecarPathForArchive(old)
				newSC := sidecarPathForArchive(newName)
				if err := atomicfs.Rename(oldSC, newSC); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("rename sidecar %s -> %s: %w", oldSC, newSC, err)
				}
			}
		}
	}
	c.part++
	var path string
	if c.part == 1 {
		path = filepath.Clean(filepath.Join(c.dir, WithZstExt(DefaultBasename(c.stamp), true)))
	} else {
		path = filepath.Clean(zstPartPath(c.dir, c.stamp, c.part))
	}
	var sink *outputSink
	var err error
	if c.indexSidecar {
		sink, err = newOutputSinkWithSidecar(path, true, c.writeSearchIdx)
	} else {
		sink, err = newOutputSink(path, true, c.writeSearchIdx)
	}
	if err != nil {
		return err
	}
	c.cur = sink
	c.linesInPart = 0
	c.paths = append(c.paths, path)
	// force-exit safety: every part registered, registry stat-filters at
	// print time so pre-rename paths are harmless once gone
	RegisterCleanupPath(path)
	// part=1 = initial sink, not rotation. only emit events from part>=2
	if c.part > 1 {
		c.dbg.Event("rotate-open: part=%d path=%s prevLines=%d", c.part, path, prevLines)
	}
	return nil
}

func (c *chunkedZstdSink) outputPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.paths))
	copy(out, c.paths)
	return out
}

func (c *chunkedZstdSink) writeBatch(buf []byte, lineCount int, m *Metrics) error {
	if lineCount <= 0 || len(buf) == 0 {
		return nil
	}
	off := 0
	remaining := lineCount
	for remaining > 0 {
		c.mu.Lock()
		room := c.chunkLines - c.linesInPart
		if room <= 0 {
			if err := c.rotateLocked(); err != nil {
				c.mu.Unlock()
				return err
			}
			room = c.chunkLines
		}
		take := int64(remaining)
		if take > room {
			take = room
		}
		nBytes, err := byteOffsetAfterNLines(buf, off, int(take))
		if err != nil {
			c.mu.Unlock()
			return err
		}
		slice := buf[off : off+nBytes]
		if err := c.cur.writeBatch(slice, int(take), m); err != nil {
			c.mu.Unlock()
			return err
		}
		c.linesInPart += take
		c.mu.Unlock()
		off += nBytes
		remaining -= int(take)
	}
	return nil
}

func (c *chunkedZstdSink) writeBatchIndexed(buf []byte, hashes []uint64, lineCount int, m *Metrics) error {
	if lineCount <= 0 || len(buf) == 0 {
		return nil
	}
	if len(hashes) != lineCount {
		return fmt.Errorf("writeBatchIndexed: %d hashes != %d lines", len(hashes), lineCount)
	}
	off := 0
	hashOff := 0
	remaining := lineCount
	for remaining > 0 {
		c.mu.Lock()
		room := c.chunkLines - c.linesInPart
		if room <= 0 {
			if err := c.rotateLocked(); err != nil {
				c.mu.Unlock()
				return err
			}
			room = c.chunkLines
		}
		take := int64(remaining)
		if take > room {
			take = room
		}
		nBytes, err := byteOffsetAfterNLines(buf, off, int(take))
		if err != nil {
			c.mu.Unlock()
			return err
		}
		slice := buf[off : off+nBytes]
		hashSlice := hashes[hashOff : hashOff+int(take)]
		if c.indexSidecar {
			if err := c.cur.writeBatchIndexed(slice, hashSlice, int(take), m); err != nil {
				c.mu.Unlock()
				return err
			}
		} else if err := c.cur.writeBatch(slice, int(take), m); err != nil {
			c.mu.Unlock()
			return err
		}
		c.linesInPart += take
		c.mu.Unlock()
		off += nBytes
		hashOff += int(take)
		remaining -= int(take)
	}
	return nil
}

func (c *chunkedZstdSink) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil {
		return nil
	}
	err := c.cur.close()
	c.cur = nil
	return err
}

// byte len from off covering exactly n full \n-terminated records
func byteOffsetAfterNLines(buf []byte, off, n int) (int, error) {
	if n == 0 {
		return 0, nil
	}
	start := off
	count := 0
	for i := off; i < len(buf); i++ {
		if buf[i] == '\n' {
			count++
			if count == n {
				return i - start + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("batch has fewer than %d newline-terminated lines", n)
}

func (s *outputSink) outputPaths() []string {
	if s == nil || s.path == "" {
		return nil
	}
	return []string{s.path}
}
