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
	// seal finalizes staged archive bytes + sidecars; publishes nothing.
	seal() error
	// commit publishes every staged artifact to its final name atomically.
	commit() error
	// abort removes only staged temps; final names are never touched.
	abort() error
	outputPaths() []string
}

// optional: -od output sinks record dedup hashes alongside archive writes
type indexedLineSink interface {
	lineSink
	writeBatchIndexed(buf []byte, hashes []uint64, lineCount int, m *Metrics) error
}

func newLineSink(r *Resolved) (lineSink, error) {
	// dry-run writes nothing: a discard sink counts unique lines + would-be
	// uncompressed bytes exactly like a real sink (so stats and the live
	// write-rate match a -od run) but creates no archive, no sidecar, and no
	// temp file. Compression CPU is skipped entirely.
	if r.Cfg.DryRun {
		return discardSink{}, nil
	}

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

// discardSink is the dry-run sink: it bumps the same counters a real sink does
// (LinesUnique, BytesWritten) so the recap stats and live write-rate match a
// live -od run, but writes nothing to disk — no archive, no sidecar, no temp
// file. The library is never touched and compression is skipped entirely.
type discardSink struct{}

func (discardSink) writeBatch(buf []byte, lineCount int, m *Metrics) error {
	if m != nil && lineCount > 0 {
		m.LinesUnique.Add(int64(lineCount))
		m.BytesWritten.Add(int64(len(buf)))
	}
	return nil
}

func (discardSink) writeBatchIndexed(buf []byte, _ []uint64, lineCount int, m *Metrics) error {
	if m != nil && lineCount > 0 {
		m.LinesUnique.Add(int64(lineCount))
		m.BytesWritten.Add(int64(len(buf)))
	}
	return nil
}

func (discardSink) seal() error           { return nil }
func (discardSink) commit() error         { return nil }
func (discardSink) abort() error          { return nil }
func (discardSink) outputPaths() []string { return nil }

// removeStagedTemp removes one staged temp file. Registration is dropped only
// once the file is really gone; a failed removal keeps the path registered so
// a force-exit can still clean it up.
func removeStagedTemp(path string) error {
	err := os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		UnregisterCleanupPath(path)
		return nil
	}
	return err
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
// sfu_<stamp>.txt.zst, _partN suffix only when >=2 archives exist
// (part 1's staged final name gains _part1 when part 2 opens — no on-disk
// rename, the staged temp is simply re-targeted before it is ever published).
// Every completed part is sealed into c.sealed and published only at commit,
// so a failed run publishes no part at all. dbg optional, rotation events go
// to debugLog.Event for timelining
type chunkedZstdSink struct {
	mu             sync.Mutex
	dir            string
	stamp          string
	chunkLines     int64
	part           int
	linesInPart    int64
	cur            *outputSink
	sealed         []*outputSink
	allSealed      bool
	dbg            *DebugLog
	writeSearchIdx bool
	indexSidecar   bool
}

func newChunkedZstdSink(dir, stamp string, chunkLines int64, dbg *DebugLog, writeSearchIdx, indexSidecar bool) (*chunkedZstdSink, error) {
	c := &chunkedZstdSink{dir: dir, stamp: stamp, chunkLines: chunkLines, dbg: dbg, writeSearchIdx: writeSearchIdx, indexSidecar: indexSidecar}
	if err := c.rotateLocked(); err != nil {
		// a partially staged first part must not outlive the failed constructor
		_ = c.abort()
		return nil, err
	}
	return c, nil
}

func (c *chunkedZstdSink) rotateLocked() error {
	prevLines := c.linesInPart
	if c.cur != nil {
		// first file -> _part1 final name once we're opening a 2nd archive.
		// staged only: the final name is re-targeted before part 1 is ever
		// sealed/published, so no final sidecar is exposed early.
		if c.part == 1 {
			old := c.cur.finalPath
			newName := filepath.Clean(zstPartPath(c.dir, c.stamp, 1))
			c.cur.retargetFinal(newName)
			c.dbg.Event("rotate-rename: part=1 %s -> %s", filepath.Base(old), filepath.Base(newName))
		}
		if err := c.cur.seal(); err != nil {
			return err
		}
		c.sealed = append(c.sealed, c.cur)
		c.cur = nil
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
	// multipart: part commits defer parent-dir syncs; the chunked commit
	// fsyncs each affected directory exactly once after the batch
	sink.deferDirSync = true
	c.cur = sink
	c.linesInPart = 0
	// part=1 = initial sink, not rotation. only emit events from part>=2
	if c.part > 1 {
		c.dbg.Event("rotate-open: part=%d path=%s prevLines=%d", c.part, path, prevLines)
	}
	return nil
}

// finalPathsLocked lists the final archive names of every staged part.
func (c *chunkedZstdSink) finalPathsLocked() []string {
	out := make([]string, 0, len(c.sealed)+1)
	for _, p := range c.sealed {
		out = append(out, p.finalPath)
	}
	if c.cur != nil {
		out = append(out, c.cur.finalPath)
	}
	return out
}

func (c *chunkedZstdSink) outputPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.finalPathsLocked()
}

// seal seals the open part (if any). Idempotent.
func (c *chunkedZstdSink) seal() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.allSealed {
		return nil
	}
	if c.cur != nil {
		if err := c.cur.seal(); err != nil {
			return err
		}
		c.sealed = append(c.sealed, c.cur)
		c.cur = nil
	}
	c.allSealed = true
	return nil
}

// commit publishes every sealed part, in part order, as ONE transaction
// (H-22). Before the first publish, every destination the batch would
// overwrite is verified to be either absent or a regular file, and existing
// regular files are renamed to same-directory backups; if any later part
// fails to publish, the failed transaction is rolled back: staged temps of
// unpublished parts are aborted, every published destination is restored to
// its exact pre-run state (backup renamed back, or removed when the path did
// not exist before), and the error reports the failure. A pre-existing
// non-regular destination (e.g. a directory) fails the whole batch up front,
// before anything is touched. Durability: each affected parent directory is
// fsynced exactly once after the batch (or after the restore on failure).
func (c *chunkedZstdSink) commit() error {
	if err := c.seal(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// parent dirs must be collected while searchStaged is still staged
	var dirSet []string
	seen := map[string]struct{}{}
	addDirs := func(parts []*outputSink) {
		for _, p := range parts {
			for _, d := range p.parentDirs() {
				if _, ok := seen[d]; ok {
					continue
				}
				seen[d] = struct{}{}
				dirSet = append(dirSet, d)
			}
		}
	}
	addDirs(c.sealed)

	// preflight: every destination this batch would overwrite must be absent
	// or a regular file. A directory (or other non-regular entry) can never
	// be atomically replaced by rename — failing here, before anything is
	// published, keeps the whole batch untouched (H-22 preflight leg).
	for i, p := range c.sealed {
		for _, dst := range p.destinationPaths() {
			if fi, err := os.Lstat(dst); err == nil && !fi.Mode().IsRegular() {
				// nothing was published: staged temps are safe to drop
				for _, q := range c.sealed {
					_ = q.abort()
				}
				return fmt.Errorf("commit: destination %s is not a regular file (%s); refusing the whole batch — remove or replace it first",
					dst, describeFileMode(fi.Mode()))
			} else if err != nil && !os.IsNotExist(err) {
				for _, q := range c.sealed {
					_ = q.abort()
				}
				return fmt.Errorf("commit: stat destination %s (part %d): %w", dst, i+1, err)
			}
		}
	}

	// snapshot: back up every existing destination before the first publish
	backups, berr := c.backupDestinations()
	if berr != nil {
		if rerr := c.restoreDestinations(backups); rerr != nil {
			return fmt.Errorf("commit: backup failed (%v) AND partial restore failed (%v)", berr, rerr)
		}
		return berr
	}

	var published int
	for i, p := range c.sealed {
		if err := p.commit(); err != nil {
			// abort the parts that were never published
			for _, q := range c.sealed[i:] {
				_ = q.abort()
			}
			// roll back the published prefix to the pre-run state (H-22)
			restoreErr := c.restoreDestinations(backups)
			_ = c.syncDirsLocked(dirSet)
			if restoreErr != nil {
				return fmt.Errorf("commit: failed on %s: %w (AND rollback failed: %v — inspect the %s directories manually)", filepath.Base(p.finalPath), err, restoreErr, filepath.Dir(p.finalPath))
			}
			return fmt.Errorf("commit: failed on %s after publishing %d of %d parts; all destinations restored to their pre-run state: %w",
				filepath.Base(p.finalPath), published, len(c.sealed), err)
		}
		published++
	}

	if err := c.syncDirsLocked(dirSet); err != nil {
		return err
	}
	if err := c.discardBackups(backups); err != nil {
		return fmt.Errorf("commit: removing superseded backups: %w", err)
	}
	return nil
}

// chunkedBackup pairs a destination with its same-directory pre-commit
// backup. Recorded only after the backup rename succeeded.
type chunkedBackup struct{ dst, bak string }

// backupDestinations renames every existing destination out of the way
// before the first publish. On failure the already-taken backups are restored
// and the error is returned (callers must not publish afterwards).
// Caller holds c.mu.
func (c *chunkedZstdSink) backupDestinations() ([]chunkedBackup, error) {
	var backups []chunkedBackup
	for _, p := range c.sealed {
		for _, dst := range p.destinationPaths() {
			if _, err := os.Lstat(dst); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				_ = c.restoreDestinations(backups)
				return nil, fmt.Errorf("commit: stat destination %s: %w", dst, err)
			}
			bak, err := reserveBackupName(dst)
			if err != nil {
				_ = c.restoreDestinations(backups)
				return nil, fmt.Errorf("commit: reserve backup for %s: %w", dst, err)
			}
			if err := atomicfs.Rename(dst, bak); err != nil {
				_ = c.restoreDestinations(backups)
				return nil, fmt.Errorf("commit: back up %s: %w", dst, err)
			}
			backups = append(backups, chunkedBackup{dst: dst, bak: bak})
		}
	}
	return backups, nil
}

// restoreDestinations rolls a failed transaction back to the pre-run state:
// published (or backed-up-aside) destinations are restored from their
// backups; destinations that did not exist before are removed. Best effort
// per entry; the first error is returned after processing every entry.
// Caller holds c.mu.
func (c *chunkedZstdSink) restoreDestinations(backups []chunkedBackup) error {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, b := range backups {
		_ = os.Remove(b.dst) // may legitimately not exist (never published)
		keep(atomicfs.Rename(b.bak, b.dst))
	}
	return firstErr
}

// discardBackups removes the superseded pre-commit backups after the whole
// batch published successfully. Caller holds c.mu.
func (c *chunkedZstdSink) discardBackups(backups []chunkedBackup) error {
	var firstErr error
	for _, b := range backups {
		if err := os.Remove(b.bak); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// destinationPaths lists every final path this sink's commit renames into
// place: the archive, its dedup .idx sidecar (when staged), and its search
// sidecar (when staged).
func (s *outputSink) destinationPaths() []string {
	paths := []string{s.finalPath}
	if s.sidecar != nil {
		paths = append(paths, sidecarPathForArchive(s.finalPath))
	}
	if s.searchStaged != "" {
		paths = append(paths, s.finalSearchSidecarPath())
	}
	return paths
}

// reserveBackupName returns an unused hidden same-directory backup name for
// dst (same-directory so restoring it is itself an atomic rename).
func reserveBackupName(dst string) (string, error) {
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".precommit-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	// the placeholder content is replaced by the backup rename immediately
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// describeFileMode renders a non-regular entry kind for error messages.
func describeFileMode(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "non-regular file"
	}
}

// syncDirsLocked fsyncs each affected parent directory exactly once per
// multipart commit batch.
func (c *chunkedZstdSink) syncDirsLocked(dirs []string) error {
	var firstErr error
	for _, d := range dirs {
		if err := durableSyncDir(d); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("sync dir %s: %w", d, err)
		}
	}
	return firstErr
}

// abort drops every staged temp (open part + sealed parts). Parts already
// committed by a partial commit are left untouched.
func (c *chunkedZstdSink) abort() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	if c.cur != nil {
		if err := c.cur.abort(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.cur = nil
	}
	for _, p := range c.sealed {
		if err := p.abort(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.sealed = nil
	return firstErr
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
	if s == nil || s.finalPath == "" {
		return nil
	}
	return []string{s.finalPath}
}
