package main

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
	writeLine(line string, m *metrics) error
	writeBatch(buf []byte, lineCount int, m *metrics) error
	close() error
	outputPaths() []string
}

func newLineSink(r *resolved) (lineSink, error) {
	writeSearchIdx := r.cfg.DestDedup && r.cfg.Compress
	if !r.cfg.Compress {
		s, err := newOutputSink(r.cfg.Output, false, false)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	if r.cfg.ZstChunkLines <= 0 {
		s, err := newOutputSink(r.cfg.Output, true, writeSearchIdx)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	chunk := r.cfg.ZstChunkLines
	dir := filepath.Dir(r.cfg.Output)
	stamp := r.cfg.RunStamp
	if stamp == "" {
		stamp = r.cfg.RunStarted.Format("20060102")
	}
	out, err := newChunkedZstdSink(dir, stamp, chunk, r.cfg.Debug, writeSearchIdx)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func sinkOutputPaths(s lineSink) []string {
	if s == nil {
		return nil
	}
	return s.outputPaths()
}

func removeOutputFiles(paths []string) {
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(p)
		}
	}
}

// removes collected inputs after success. skips paths matching outputs
// (after Abs+Clean and inode check). returns partial list on err
func deleteParsedInputs(inputs, outputs []string) ([]string, error) {
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
	dbg            *debugLog
	writeSearchIdx bool
}

func newChunkedZstdSink(dir, stamp string, chunkLines int64, dbg *debugLog, writeSearchIdx bool) (*chunkedZstdSink, error) {
	c := &chunkedZstdSink{dir: dir, stamp: stamp, chunkLines: chunkLines, dbg: dbg, writeSearchIdx: writeSearchIdx}
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
			registerCleanupPath(newName)
			c.dbg.Event("rotate-rename: part=1 %s -> %s", old, newName)
		}
	}
	c.part++
	var path string
	if c.part == 1 {
		path = filepath.Clean(filepath.Join(c.dir, withZstExt(defaultBasename(c.stamp), true)))
	} else {
		path = filepath.Clean(zstPartPath(c.dir, c.stamp, c.part))
	}
	sink, err := newOutputSink(path, true, c.writeSearchIdx)
	if err != nil {
		return err
	}
	c.cur = sink
	c.linesInPart = 0
	c.paths = append(c.paths, path)
	// force-exit safety: every part registered, registry stat-filters at
	// print time so pre-rename paths are harmless once gone
	registerCleanupPath(path)
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

func (c *chunkedZstdSink) writeLine(line string, m *metrics) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.linesInPart >= c.chunkLines {
		if err := c.rotateLocked(); err != nil {
			return err
		}
	}
	err := c.cur.writeLine(line, m)
	if err == nil {
		c.linesInPart++
	}
	return err
}

func (c *chunkedZstdSink) writeBatch(buf []byte, lineCount int, m *metrics) error {
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
