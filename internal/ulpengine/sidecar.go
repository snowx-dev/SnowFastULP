package ulpengine

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
)

// .idx sidecar layout at <archiveDir>/<idxSubdirName>/<basename>.idx.
// xxhash64 fingerprint index (NOT crypto). "key" means dedup hash key.
//
//	0..3    "SFIX"                magic
//	4..5    u16 LE                format version
//	6..7    u16 LE                hash algo id (0 = xxhash64)
//	8..15   u64 LE                keyCount
//	16..23  u64 LE                parserVersion
//	24..31  reserved (zeros)      [base header ends at 32 bytes]
//	32..39  i64 LE                bound archive size (v4)
//	40..71  32 B                  bound archive SHA-256 (v4)
//	72..79  i64 LE                verified instance mtime, 0 = unconfirmed (v4)
//	80..87  u64 LE                verified instance dev, 0 = none (v4)
//	88..95  u64 LE                verified instance inode, 0 = none (v4)
//	96..    u64 LE × keyCount     packed dedup hashes
//
// LE everywhere matches the xxhash uint64 storage path. one .idx per
// ARCHIVE PART, not per logical run: touching one part of a 16-part run
// invalidates only that part.
//
// format versions:
//
//	v2 = keys in archive read order (legacy). no identity binding.
//	v3 = keys SORTED ascending + deduped (legacy). no identity binding.
//	v4 = archive-identity binding (H-21): the 32-byte base header is extended
//	     to 96 bytes with the bound archive's size + SHA-256, plus a verify
//	     cache (mtime/dev/ino of the last digest-confirmed archive instance)
//	     that keeps steady-state runs from re-hashing the library. v2/v3
//	     sidecars carry no binding — their keys cannot be verified against
//	     the archive — so they are stale by definition and regenerate
//	     (one-time decompress). No in-place migration.
const (
	sidecarMagic           = "SFIX"
	sidecarFormatV4        = 4 // sorted + archive-identity bound
	sidecarFormatVer       = sidecarFormatV4
	sidecarHashAlgoXX      = 0
	sidecarBaseHeaderBytes = 32 // legacy header prefix, parsed only to detect staleness
	sidecarHeaderBytes     = 96 // v4: base + size(8) + sha256(32) + verify(24)
	SidecarKeyBytes        = 8
	sidecarSuffix          = ".idx"

	// holds all .idx for archives in the same dir. created on demand,
	// safe to delete manually (next -od run regens)
	idxSubdirName = "sfu_dedup_idx"

	// identifies parse.go + lineFormatter.HashKey rules. bump on any
	// change that affects the dedup key preimage: host derivation,
	// localhost rejection, the field encoding.
	// mismatch = errSidecarStale = silent regen on next run
	//
	// v2: regen switched from loose to parseUnion (strict OR loose). loose
	// regen dropped strict-only creds (host:user:{"uid":...}), leaving gaps
	// that re-ingest re-emitted as stragglers. bumping forces a one-time full
	// rebuild of every existing -od index with the complete union parser.
	//
	// v3: android:// creds are now parsed and keyed on the full line (see
	// parseAndroid). host derivation changed for that scheme, so every existing
	// -od index regenerates once to pick up the newly-admitted keys.
	//
	// v4: sidecar regen + FormatRecordStable verify use parseStored (FormatRecord
	// inverse, no isLikelyJunk). finishParse rejects ':' in login and empty
	// login/password. Always-verify replaces the unsound ≤2-colon skip so
	// custom-parser lines (spaced/$-logins, etc.) index correctly.
	//
	// v5: dedup key preimage replaced the ambiguous host:login:password
	// colon-join with a versioned, length-prefixed field encoding (see
	// appendDedupKeyFields), and the strict scanner consumes colons in
	// scheme:// URL paths (see parseStrictULP) — both change existing keys.
	// Bumped NOW for the key-format change rather than once together with
	// P6-W12 as the plan originally scheduled: P6 is a later phase and the
	// unambiguous encoding cannot wait. Existing .idx sidecars regenerate; there is no
	// in-place migration.
	//
	// v6: the canonical host now folds hostname case (P6-W12, RR-6.12) so
	// Example.COM and example.com share one library key. The plan's original
	// "stay at 5" was superseded: the key inputs change again, so old v5
	// sidecars must regenerate once (v5 sidecars never shipped in a release
	// together with this tree). No in-place migration; existing .idx
	// regenerate on the next -od run.
	parserVersion uint64 = 6
)

// in-RAM sort budget per sidecar writer before spilling to external-merge runs.
// peak sort RAM ≈ regenWorkers × this. small on purpose: sfu's core promise is
// bounded memory, and -split-zst 0 parts can be billions of keys. var (not
// const) so tests can force the spill/merge path with a tiny budget.
var sidecarSortMaxKeys = (32 << 20) / SidecarKeyBytes // 32 MiB of keys

var (
	errSidecarMissing   = errors.New("sidecar: not found")
	errSidecarMalformed = errors.New("sidecar: malformed header")
	errSidecarStale     = errors.New("sidecar: format/parser version mismatch")
)

type sidecarHeader struct {
	formatVersion uint16
	hashAlgo      uint16
	keyCount      uint64
	parserVersion uint64
	// v4 archive binding (see sidecarHeaderBytes): the identity of the
	// archive this .idx was built from, plus the last instance whose digest
	// was confirmed (the verify cache that keeps steady-state runs cheap).
	archiveSize     int64
	sha             [32]byte
	verifyMtimeNano int64
	verifyDev       uint64
	verifyIno       uint64
}

// <archive>.idx under the sibling idxSubdirName subdir. callers needing
// the subdir created call ensureIdxSubdir(filepath.Dir(archivePath))
func sidecarPathForArchive(archivePath string) string {
	dir := filepath.Dir(archivePath)
	base := filepath.Base(archivePath)
	return filepath.Join(dir, idxSubdirName, base+sidecarSuffix)
}

// idempotent, safe under regen-pool concurrency
func ensureIdxSubdir(archiveDir string) error {
	return os.MkdirAll(filepath.Join(archiveDir, idxSubdirName), 0o755)
}

// reads + validates the v4 header (96 bytes). errSidecarMissing/Malformed/
// Stale returned for the 3 fail cases. cheap stat-equivalent for discovery.
// legacy v2/v3 sidecars parse far enough to be classified errSidecarStale —
// they carry no archive-identity binding, so their keys cannot be verified
// against the archive and the scan regenerates them.
func readSidecarHeader(path string) (*sidecarHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errSidecarMissing
		}
		return nil, err
	}
	defer f.Close()

	var base [sidecarBaseHeaderBytes]byte
	if _, err := io.ReadFull(f, base[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", errSidecarMalformed, err)
	}
	if string(base[0:4]) != sidecarMagic {
		return nil, errSidecarMalformed
	}

	h := &sidecarHeader{
		formatVersion: binary.LittleEndian.Uint16(base[4:6]),
		hashAlgo:      binary.LittleEndian.Uint16(base[6:8]),
		keyCount:      binary.LittleEndian.Uint64(base[8:16]),
		parserVersion: binary.LittleEndian.Uint64(base[16:24]),
	}

	// only v4 is readable: v2/v3 lack the archive-identity binding (their
	// keys cannot be proven to describe the current archive), and any
	// parser-rule or hash-algo change forces a full regen as before.
	if h.formatVersion != sidecarFormatV4 ||
		h.hashAlgo != sidecarHashAlgoXX ||
		h.parserVersion != parserVersion {
		return h, errSidecarStale
	}

	var ext [sidecarHeaderBytes - sidecarBaseHeaderBytes]byte
	if _, err := io.ReadFull(f, ext[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", errSidecarMalformed, err)
	}
	h.archiveSize = int64(binary.LittleEndian.Uint64(ext[0:8]))
	copy(h.sha[:], ext[8:40])
	h.verifyMtimeNano = int64(binary.LittleEndian.Uint64(ext[40:48]))
	h.verifyDev = binary.LittleEndian.Uint64(ext[48:56])
	h.verifyIno = binary.LittleEndian.Uint64(ext[56:64])
	if h.archiveSize < 0 {
		return h, fmt.Errorf("%w: negative archive size", errSidecarMalformed)
	}

	// body size = header + keyCount*8. defence vs truncated writes
	fi, err := f.Stat()
	if err != nil {
		return h, err
	}
	if fi.Size() < sidecarHeaderBytes {
		return h, fmt.Errorf("%w: size %d < header %d", errSidecarMalformed, fi.Size(), sidecarHeaderBytes)
	}
	bodyBytes := fi.Size() - sidecarHeaderBytes
	if bodyBytes%SidecarKeyBytes != 0 {
		return h, fmt.Errorf("%w: body size %d not multiple of %d", errSidecarMalformed, bodyBytes, SidecarKeyBytes)
	}
	wantKeys := uint64(bodyBytes / SidecarKeyBytes)
	if h.keyCount != wantKeys {
		return h, fmt.Errorf("%w: keyCount %d != body keys %d", errSidecarMalformed, h.keyCount, wantKeys)
	}
	return h, nil
}

// validates header + calls fn per key. ~64 KiB chunks so RAM stays bounded
// regardless of key count. aborts on first fn err
func streamSidecarKeys(path string, fn func(uint64) error) error {
	return streamSidecarKeyBytes(path, 64*1024, func(raw []byte) error {
		for off := 0; off < len(raw); off += SidecarKeyBytes {
			if err := fn(binary.LittleEndian.Uint64(raw[off : off+SidecarKeyBytes])); err != nil {
				return err
			}
		}
		return nil
	})
}

// fast path for -od routing. calls fn w/ key-aligned raw byte chunks so
// reading many keys per io.ReadFull avoids billions of tiny callbacks
func streamSidecarKeyBytes(path string, chunkBytes int, fn func([]byte) error) error {
	hdr, err := readSidecarHeader(path)
	if err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(sidecarHeaderBytes, io.SeekStart); err != nil {
		return err
	}

	if chunkBytes < SidecarKeyBytes {
		chunkBytes = SidecarKeyBytes
	}
	chunkBytes -= chunkBytes % SidecarKeyBytes
	if chunkBytes == 0 {
		chunkBytes = SidecarKeyBytes
	}

	br := bufio.NewReaderSize(f, chunkBytes)
	buf := make([]byte, chunkBytes)
	keysRemaining := hdr.keyCount
	keysPerChunk := uint64(chunkBytes / SidecarKeyBytes)
	keysRead := uint64(0)
	for keysRemaining > 0 {
		nKeys := keysPerChunk
		if nKeys > keysRemaining {
			nKeys = keysRemaining
		}
		nBytes := int(nKeys) * SidecarKeyBytes
		if _, err := io.ReadFull(br, buf[:nBytes]); err != nil {
			return fmt.Errorf("sidecar: read keys %d-%d/%d: %w", keysRead, keysRead+nKeys, hdr.keyCount, err)
		}
		if err := fn(buf[:nBytes]); err != nil {
			return err
		}
		keysRead += nKeys
		keysRemaining -= nKeys
	}
	return nil
}

// builds a SORTED+deduped .idx (v3). keys are buffered in RAM up to
// sidecarSortMaxKeys, then spilled to sorted run files; Commit k-way merges the
// runs (or just sorts the single in-RAM batch) into the final body. RAM stays
// bounded regardless of part size. temp + fsync + atomic rename.
type sidecarWriter struct {
	finalPath string
	dir       string
	tmpPath   string
	f         *os.File
	buf       []uint64
	spills    []string
	// archiveSource is the file whose bytes the .idx describes: the archive
	// itself for regen, the staged temp for output sinks (commit renames it
	// to the final archive unchanged, so the identity describes both — same
	// principle as the search sidecar). Read at finish() to bind the v4
	// header to the archive's size + SHA-256 (H-21).
	archiveSource string
}

func newSidecarWriter(archivePath string) (*sidecarWriter, error) {
	if err := ensureIdxSubdir(filepath.Dir(archivePath)); err != nil {
		return nil, fmt.Errorf("sidecar: mkdir %s: %w", idxSubdirName, err)
	}
	// the archive exists and is final here (regen path): bind to it directly.
	return newSidecarWriterAtPath(sidecarPathForArchive(archivePath), archivePath)
}

// newSidecarWriterAtPath builds a writer targeting an exact .idx path (the
// parent dir must already exist). archiveSource names the file whose bytes
// the .idx describes and is read at finish(); output sinks re-point it at
// their staged temp via SetArchiveSource before sealing.
func newSidecarWriterAtPath(finalPath, archiveSource string) (*sidecarWriter, error) {
	dir := filepath.Dir(finalPath)
	// O_EXCL random name = no symlink clobbering in shared dirs
	tmp, err := os.CreateTemp(dir, filepath.Base(finalPath)+".write.*.tmp")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	RegisterCleanupPath(tmpPath)

	var headerPlaceholder [sidecarHeaderBytes]byte
	if _, err := tmp.Write(headerPlaceholder[:]); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		UnregisterCleanupPath(tmpPath)
		return nil, err
	}

	return &sidecarWriter{finalPath: finalPath, dir: dir, tmpPath: tmpPath, f: tmp, archiveSource: archiveSource}, nil
}

// SetArchiveSource re-points the identity source at another path holding the
// same bytes the final archive will have (the staged temp). Valid only
// before finish().
func (w *sidecarWriter) SetArchiveSource(path string) {
	if w != nil {
		w.archiveSource = path
	}
}

func (w *sidecarWriter) WriteHash(k uint64) error {
	if w == nil || w.f == nil {
		return fmt.Errorf("sidecar: writer closed")
	}
	w.buf = append(w.buf, k)
	if len(w.buf) >= sidecarSortMaxKeys {
		return w.spill()
	}
	return nil
}

// sort+compact the in-RAM batch to a temp run file, reset the buffer.
func (w *sidecarWriter) spill() error {
	if len(w.buf) == 0 {
		return nil
	}
	slices.Sort(w.buf)
	w.buf = slices.Compact(w.buf)
	runPath, err := writeKeyRun(w.dir, w.buf)
	if err != nil {
		return err
	}
	RegisterCleanupPath(runPath)
	w.spills = append(w.spills, runPath)
	w.buf = w.buf[:0]
	return nil
}

// finish builds the final sidecar body (sorted+deduped) into the writer's temp
// file, writes the header, syncs, and closes the temp. The sidecar is NOT
// published: publish() performs the rename. This lets the output sink stage
// sidecars at seal time and publish them at commit time. Returns the unique
// key count.
func (w *sidecarWriter) finish() (uint64, error) {
	if w == nil || w.f == nil {
		return 0, fmt.Errorf("sidecar: writer closed")
	}
	bw := bufio.NewWriterSize(w.f, 1<<20)
	var count uint64
	var err error
	if len(w.spills) == 0 {
		// in-RAM fast path: the part never exceeded the sort budget.
		slices.Sort(w.buf)
		w.buf = slices.Compact(w.buf)
		count, err = writeKeysSorted(bw, w.buf)
	} else {
		// flush the tail batch, then k-way merge all runs (deduped).
		if err = w.spill(); err == nil {
			count, err = mergeKeyRuns(bw, w.spills)
		}
	}
	if err != nil {
		_ = w.Abort()
		return 0, err
	}
	if err = bw.Flush(); err != nil {
		_ = w.Abort()
		return 0, err
	}
	// H-21: bind the v4 header to the archive's content. The source is the
	// archive itself (regen) or the staged temp (output sinks); commit's
	// rename preserves size/mtime/inode, so the identity also describes the
	// published archive — the same principle the search sidecar documents.
	ident, ierr := archiveSourceIdentity(w.archiveSource)
	if ierr != nil {
		_ = w.Abort()
		return 0, fmt.Errorf("sidecar: archive identity %s: %w", w.archiveSource, ierr)
	}
	var sha [32]byte
	if _, derr := hex.Decode(sha[:], []byte(ident.SHA256)); derr != nil {
		_ = w.Abort()
		return 0, fmt.Errorf("sidecar: archive digest: %w", derr)
	}
	var verifyMtimeNano int64
	var verifyDev, verifyIno uint64
	if afi, aerr := os.Stat(w.archiveSource); aerr == nil {
		verifyMtimeNano = afi.ModTime().UnixNano()
		verifyDev, verifyIno, _ = archiveInstanceDevIno(afi)
	}
	header := makeSidecarHeader(count, ident.Size, sha, verifyMtimeNano, verifyDev, verifyIno)
	if _, err = w.f.WriteAt(header[:], 0); err != nil {
		_ = w.Abort()
		return 0, err
	}
	// durability: fsync the staged .idx before close so the published
	// sidecar never exposes unwritten bytes
	if err = durableSyncFile(w.tmpPath); err != nil {
		_ = w.Abort()
		return 0, err
	}
	if err = w.f.Close(); err != nil {
		w.f = nil
		// drop the temp only when removal actually left it absent; a surviving
		// file stays registered so a force-exit can still clean it up
		if rerr := os.Remove(w.tmpPath); rerr == nil || os.IsNotExist(rerr) {
			UnregisterCleanupPath(w.tmpPath)
		}
		w.removeSpills()
		return 0, err
	}
	w.f = nil
	return count, nil
}

// publish renames the finished temp sidecar into its final path. The temp must
// have been closed by finish(). On failure the temp stays on disk (and
// cleanup-registered) so a later abort can still remove it.
func (w *sidecarWriter) publish() error {
	if w == nil {
		return nil
	}
	if w.f != nil {
		return fmt.Errorf("sidecar: publish before finish")
	}
	if err := atomicfs.Rename(w.tmpPath, w.finalPath); err != nil {
		return err
	}
	// committed: the temp is now the final sidecar, not a deletable scratch file
	UnregisterCleanupPath(w.tmpPath)
	w.removeSpills()
	return nil
}

func (w *sidecarWriter) Commit() (uint64, error) {
	count, err := w.finish()
	if err != nil {
		return 0, err
	}
	if err = w.publish(); err != nil {
		// publish failed: drop the temp exactly like the old rename-failure path
		if rerr := os.Remove(w.tmpPath); rerr == nil || os.IsNotExist(rerr) {
			UnregisterCleanupPath(w.tmpPath)
		}
		w.removeSpills()
		return 0, err
	}
	return count, nil
}

func (w *sidecarWriter) Abort() error {
	if w == nil {
		return nil
	}
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	w.removeSpills()
	err := os.Remove(w.tmpPath)
	if os.IsNotExist(err) {
		UnregisterCleanupPath(w.tmpPath)
		return nil
	}
	if err == nil {
		UnregisterCleanupPath(w.tmpPath)
	}
	// removal failed and the file still exists: keep it registered so a
	// force-exit can still clean it up
	return err
}

// removeSpills removes every spill run file. Registration is dropped per path
// as soon as the file is actually gone; a failed removal keeps the path
// registered so a force-exit can still clean it up.
func (w *sidecarWriter) removeSpills() {
	for _, p := range w.spills {
		if err := os.Remove(p); err == nil || os.IsNotExist(err) {
			UnregisterCleanupPath(p)
		}
	}
	w.spills = nil
}

// writeKeysSorted writes already-sorted keys as a packed u64 LE body.
func writeKeysSorted(bw *bufio.Writer, keys []uint64) (uint64, error) {
	var b [SidecarKeyBytes]byte
	for _, k := range keys {
		binary.LittleEndian.PutUint64(b[:], k)
		if _, err := bw.Write(b[:]); err != nil {
			return 0, err
		}
	}
	return uint64(len(keys)), nil
}

// writeKeyRun spills sorted keys to a fresh temp run file (raw u64 LE).
func writeKeyRun(dir string, keys []uint64) (string, error) {
	f, err := os.CreateTemp(dir, "sfu_idxrun.*.tmp")
	if err != nil {
		return "", err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	if _, werr := writeKeysSorted(bw, keys); werr != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", werr
	}
	if ferr := bw.Flush(); ferr != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", ferr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(f.Name())
		return "", cerr
	}
	return f.Name(), nil
}

// streaming reader over one sorted run file
type keyRunReader struct {
	f   *os.File
	br  *bufio.Reader
	cur uint64
	ok  bool
}

func openKeyRun(path string) (*keyRunReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &keyRunReader{f: f, br: bufio.NewReaderSize(f, 256<<10)}
	if err := r.advance(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

func (r *keyRunReader) advance() error {
	var b [SidecarKeyBytes]byte
	if _, err := io.ReadFull(r.br, b[:]); err != nil {
		r.ok = false
		if err == io.EOF {
			return nil
		}
		return err
	}
	r.cur = binary.LittleEndian.Uint64(b[:])
	r.ok = true
	return nil
}

func (r *keyRunReader) close() {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

// min-heap of run readers keyed by current value
type runHeap []*keyRunReader

func (h runHeap) Len() int           { return len(h) }
func (h runHeap) Less(i, j int) bool { return h[i].cur < h[j].cur }
func (h runHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(x any)        { *h = append(*h, x.(*keyRunReader)) }
func (h *runHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// mergeKeyRuns k-way merges sorted run files into bw (deduped, ascending) and
// returns the unique key count. bounded RAM: one buffered reader per run.
func mergeKeyRuns(bw *bufio.Writer, runPaths []string) (uint64, error) {
	h := &runHeap{}
	readers := make([]*keyRunReader, 0, len(runPaths))
	defer func() {
		for _, r := range readers {
			r.close()
		}
	}()
	for _, p := range runPaths {
		r, err := openKeyRun(p)
		if err != nil {
			return 0, err
		}
		readers = append(readers, r)
		if r.ok {
			*h = append(*h, r)
		}
	}
	heap.Init(h)

	var count, last uint64
	have := false
	var b [SidecarKeyBytes]byte
	for h.Len() > 0 {
		top := (*h)[0]
		k := top.cur
		if !have || k != last {
			binary.LittleEndian.PutUint64(b[:], k)
			if _, err := bw.Write(b[:]); err != nil {
				return 0, err
			}
			count++
			last = k
			have = true
		}
		if err := top.advance(); err != nil {
			return 0, err
		}
		if top.ok {
			heap.Fix(h, 0)
		} else {
			heap.Pop(h)
		}
	}
	return count, nil
}

// sidecarReader is an open, header-validated v3 sidecar. Used one-shot:
// gatherDestBucketKeys opens, range-reads one bucket, and closes it before
// touching the next sidecar, keeping the process's concurrent dest-sidecar
// descriptors bounded (no EMFILE late in a big multi-part run).
type sidecarReader struct {
	path     string
	f        *os.File
	keyCount int64
}

func openSidecarReader(path string) (*sidecarReader, error) {
	hdr, err := readSidecarHeader(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &sidecarReader{path: path, f: f, keyCount: int64(hdr.keyCount)}, nil
}

func (sr *sidecarReader) close() {
	if sr != nil && sr.f != nil {
		_ = sr.f.Close()
		sr.f = nil
	}
}

// bucketRange locates one bucket's key span in the sorted on-disk body by
// binary search (positioned ReadAt — no full read). Returns the [loIdx, hiIdx)
// half-open range; the last bucket runs to EOF (its hi would overflow 1<<64).
// numBuckets must be a power of two; callers fail fast on that before reaching
// here. ctx is polled inside the binary-search reads.
func (sr *sidecarReader) bucketRange(ctx context.Context, bucketIdx, numBuckets int) (int64, int64, error) {
	if sr.keyCount == 0 {
		return 0, 0, nil
	}
	keyAt := func(i int64) (uint64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var b [SidecarKeyBytes]byte
		if _, rerr := sr.f.ReadAt(b[:], sidecarHeaderBytes+i*SidecarKeyBytes); rerr != nil {
			return 0, rerr
		}
		return binary.LittleEndian.Uint64(b[:]), nil
	}

	lo, hi, toEnd := bucketKeyRange(bucketIdx, numBuckets)
	loIdx, err := lowerBoundKey(sr.keyCount, lo, keyAt)
	if err != nil {
		return 0, 0, err
	}
	hiIdx := sr.keyCount // last bucket runs to EOF (its hi would overflow 1<<64)
	if !toEnd {
		if hiIdx, err = lowerBoundKey(sr.keyCount, hi, keyAt); err != nil {
			return 0, 0, err
		}
	}
	return loIdx, hiIdx, nil
}

// decodeBucketRange decodes the sidecar's key range starting at loIdx directly
// into dst, which must be exactly the range's length. Reads fixed-size blocks
// into a bounded scratch buffer — there is no full-range raw byte copy: the
// gather's peak storage is the 8 B/key backing slice plus this scratch. Every
// block read must return exactly the requested bytes; ReadAt may legitimately
// report io.EOF when the last byte lands on EOF, so we key off the byte count,
// not the error — a short read means the file was truncated under us and the
// tail would silently decode as key 0 — fail loud. ctx is polled per block.
func (sr *sidecarReader) decodeBucketRange(ctx context.Context, dst []uint64, loIdx int64) error {
	const blockKeys = 1 << 13 // 8192 keys = 64 KiB of read scratch
	block := len(dst)
	if block > blockKeys {
		block = blockKeys
	}
	scratch := make([]byte, block*SidecarKeyBytes)
	off := 0
	for off < len(dst) {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := len(dst) - off
		if n > blockKeys {
			n = blockKeys
		}
		want := n * SidecarKeyBytes
		got, rerr := sr.f.ReadAt(scratch[:want], sidecarHeaderBytes+(loIdx+int64(off))*SidecarKeyBytes)
		if got != want {
			if rerr == nil {
				rerr = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("sidecar: short read of bucket range at key %d (%d/%d bytes): %w", loIdx+int64(off), got, want, rerr)
		}
		for i := range n {
			dst[off+i] = binary.LittleEndian.Uint64(scratch[i*SidecarKeyBytes:])
		}
		off += n
	}
	return nil
}

// lowerBoundKey returns the first index in [0,n) whose key >= target (or n).
func lowerBoundKey(n int64, target uint64, keyAt func(int64) (uint64, error)) (int64, error) {
	lo, hi := int64(0), n
	for lo < hi {
		mid := lo + (hi-lo)/2
		k, err := keyAt(mid)
		if err != nil {
			return 0, err
		}
		if k < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// 96-byte canonical v4 header: the 32-byte base (magic, format, algo,
// keyCount, parserVersion, reserved) plus the archive-identity binding and
// the verify cache (H-21).
func makeSidecarHeader(keyCount uint64, archiveSize int64, sha [32]byte, verifyMtimeNano int64, verifyDev, verifyIno uint64) [sidecarHeaderBytes]byte {
	var h [sidecarHeaderBytes]byte
	copy(h[0:4], sidecarMagic)
	binary.LittleEndian.PutUint16(h[4:6], sidecarFormatVer)
	binary.LittleEndian.PutUint16(h[6:8], sidecarHashAlgoXX)
	binary.LittleEndian.PutUint64(h[8:16], keyCount)
	binary.LittleEndian.PutUint64(h[16:24], parserVersion)
	// bytes 24..31 reserved (zero)
	binary.LittleEndian.PutUint64(h[32:40], uint64(archiveSize))
	copy(h[40:72], sha[:])
	binary.LittleEndian.PutUint64(h[72:80], uint64(verifyMtimeNano))
	binary.LittleEndian.PutUint64(h[80:88], verifyDev)
	binary.LittleEndian.PutUint64(h[88:96], verifyIno)
	return h
}
