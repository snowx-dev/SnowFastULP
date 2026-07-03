package ulpengine

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/binary"
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
//	24..31  reserved (zeros)
//	32..    u64 LE × keyCount     packed dedup hashes
//
// LE everywhere matches the xxhash uint64 storage path. one .idx per
// ARCHIVE PART, not per logical run: touching one part of a 16-part run
// invalidates only that part.
//
// format versions:
//
//	v2 = keys in archive read order (legacy). still readable + upgradable.
//	v3 = keys SORTED ascending + deduped. lets -od read one bucket's keys as a
//	     contiguous, seekable range (with top-bits range partitioning) instead
//	     of re-routing the whole library every run.
const (
	sidecarMagic       = "SFIX"
	sidecarFormatV2    = 2 // legacy, unsorted
	sidecarFormatV3    = 3 // sorted ascending + deduped
	sidecarFormatVer   = sidecarFormatV3
	sidecarHashAlgoXX  = 0
	sidecarHeaderBytes = 32
	SidecarKeyBytes    = 8
	sidecarSuffix      = ".idx"

	// holds all .idx for archives in the same dir. created on demand,
	// safe to delete manually (next -od run regens)
	idxSubdirName = "sfu_dedup_idx"

	// identifies parse.go + lineFormatter.HashKey rules. bump on any
	// change that affects xxhash64(host:login:password): host derivation,
	// localhost rejection, the colon-join format.
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
	parserVersion uint64 = 3
)

// in-RAM sort budget per sidecar writer before spilling to external-merge runs.
// peak sort RAM ≈ regenWorkers × this. small on purpose: sfu's core promise is
// bounded memory, and -split-zst 0 parts can be billions of keys. var (not
// const) so tests can force the spill/merge path with a tiny budget.
var sidecarSortMaxKeys = (32 << 20) / SidecarKeyBytes // 32 MiB of keys

// cancel poll interval during v2→v3 upgrade (bit mask on key index). default
// ~every 256K keys. tests may lower for mid-stream cancel coverage.
var sidecarUpgradeCancelCheckMask uint64 = 0x3ffff

// sidecarUpgradeOnCancelCheck, when non-nil, is invoked at each cancel-poll
// during a v2→v3 upgrade (just before ctx is checked). Test-only: lets a test
// cancel deterministically mid-stream instead of racing a wall-clock sleep.
var sidecarUpgradeOnCancelCheck func()

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
}

// sorted reports whether the body keys are sorted ascending (v3+). v2 sidecars
// are valid but must be upgraded before range reads.
func (h *sidecarHeader) sorted() bool { return h != nil && h.formatVersion >= sidecarFormatV3 }

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

// reads + validates 32-byte header. errSidecarMissing/Malformed/Stale
// returned for the 3 fail cases. cheap stat-equivalent for discovery
func readSidecarHeader(path string) (*sidecarHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errSidecarMissing
		}
		return nil, err
	}
	defer f.Close()

	var raw [sidecarHeaderBytes]byte
	if _, err := io.ReadFull(f, raw[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", errSidecarMalformed, err)
	}
	if string(raw[0:4]) != sidecarMagic {
		return nil, errSidecarMalformed
	}

	h := &sidecarHeader{
		formatVersion: binary.LittleEndian.Uint16(raw[4:6]),
		hashAlgo:      binary.LittleEndian.Uint16(raw[6:8]),
		keyCount:      binary.LittleEndian.Uint64(raw[8:16]),
		parserVersion: binary.LittleEndian.Uint64(raw[16:24]),
	}

	// both v2 (unsorted, upgradable) and v3 (sorted) are readable. only the
	// hash algo or a parser-rule change forces a full regen.
	if (h.formatVersion != sidecarFormatV2 && h.formatVersion != sidecarFormatV3) ||
		h.hashAlgo != sidecarHashAlgoXX ||
		h.parserVersion != parserVersion {
		return h, errSidecarStale
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
}

func newSidecarWriter(archivePath string) (*sidecarWriter, error) {
	if err := ensureIdxSubdir(filepath.Dir(archivePath)); err != nil {
		return nil, fmt.Errorf("sidecar: mkdir %s: %w", idxSubdirName, err)
	}
	return newSidecarWriterAtPath(sidecarPathForArchive(archivePath))
}

// newSidecarWriterAtPath builds a writer targeting an exact .idx path (the
// parent dir must already exist). used by the v2->v3 in-place upgrade.
func newSidecarWriterAtPath(finalPath string) (*sidecarWriter, error) {
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
		return nil, err
	}

	return &sidecarWriter{finalPath: finalPath, dir: dir, tmpPath: tmpPath, f: tmp}, nil
}

// upgradeSidecarToV3 re-sorts a legacy v2 (unsorted) sidecar into v3 IN PLACE,
// without touching the archive (no decompression). Bounded RAM via the writer's
// spill/merge. No-op if already sorted. This is the transparent, one-time
// migration path triggered the first time -od sees an old sidecar.
func upgradeSidecarToV3(ctx context.Context, sidecarPath string) (uint64, error) {
	hdr, err := readSidecarHeader(sidecarPath)
	if err != nil {
		return 0, err
	}
	if hdr.sorted() {
		return hdr.keyCount, nil
	}
	w, err := newSidecarWriterAtPath(sidecarPath)
	if err != nil {
		return 0, err
	}
	// honor cancellation mid-stream so a Ctrl+C during migration responds
	// promptly even on a huge single sidecar. Abort drops the temp and leaves
	// the original v2 .idx untouched — the library is never half-written.
	n := 0
	feed := func(k uint64) error {
		if sidecarUpgradeCancelCheckMask != 0 && uint64(n)&sidecarUpgradeCancelCheckMask == 0 {
			if sidecarUpgradeOnCancelCheck != nil {
				sidecarUpgradeOnCancelCheck()
			}
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		n++
		return w.WriteHash(k)
	}
	if serr := streamSidecarKeys(sidecarPath, feed); serr != nil {
		_ = w.Abort()
		return 0, serr
	}
	return w.Commit()
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

func (w *sidecarWriter) Commit() (uint64, error) {
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
	header := makeSidecarHeader(count)
	if _, err = w.f.WriteAt(header[:], 0); err != nil {
		_ = w.Abort()
		return 0, err
	}
	if err = w.f.Sync(); err != nil {
		_ = w.Abort()
		return 0, err
	}
	if err = w.f.Close(); err != nil {
		w.f = nil
		_ = os.Remove(w.tmpPath)
		w.removeSpills()
		return 0, err
	}
	w.f = nil

	if err = atomicfs.Rename(w.tmpPath, w.finalPath); err != nil {
		_ = os.Remove(w.tmpPath)
		w.removeSpills()
		return 0, err
	}
	w.removeSpills()
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
		return nil
	}
	return err
}

func (w *sidecarWriter) removeSpills() {
	for _, p := range w.spills {
		_ = os.Remove(p)
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

// sidecarReader is an open, header-validated v3 sidecar. A dedup worker keeps
// ONE open per library sidecar and range-reads many buckets through it, so each
// sidecar is opened once per worker instead of once per (worker × bucket).
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
	if !hdr.sorted() {
		return nil, fmt.Errorf("sidecar %s is v%d (unsorted); upgrade before range read", path, hdr.formatVersion)
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

// bucketKeys returns one bucket's keys, located by binary search over the sorted
// on-disk keys (positioned ReadAt — no full read). The bucket's hash range comes
// from bucketKeyRange (shared with the shard-side partition). numBuckets must be
// a power of two; callers fail fast on that before reaching here.
func (sr *sidecarReader) bucketKeys(bucketIdx, numBuckets int) ([]uint64, error) {
	if sr.keyCount == 0 {
		return nil, nil
	}
	n := sr.keyCount
	keyAt := func(i int64) (uint64, error) {
		var b [SidecarKeyBytes]byte
		if _, rerr := sr.f.ReadAt(b[:], sidecarHeaderBytes+i*SidecarKeyBytes); rerr != nil {
			return 0, rerr
		}
		return binary.LittleEndian.Uint64(b[:]), nil
	}

	lo, hi, toEnd := bucketKeyRange(bucketIdx, numBuckets)
	loIdx, err := lowerBoundKey(n, lo, keyAt)
	if err != nil {
		return nil, err
	}
	hiIdx := n // last bucket runs to EOF (its hi would overflow 1<<64)
	if !toEnd {
		if hiIdx, err = lowerBoundKey(n, hi, keyAt); err != nil {
			return nil, err
		}
	}
	if hiIdx <= loIdx {
		return nil, nil
	}

	cnt := hiIdx - loIdx
	raw := make([]byte, cnt*SidecarKeyBytes)
	// A full read must return exactly len(raw) bytes. ReadAt may legitimately
	// report io.EOF when the last byte lands on EOF (last-bucket case), so we
	// key off the byte count, not the error: a short read means the file was
	// truncated under us and the tail would silently decode as key 0 — fail loud.
	got, rerr := sr.f.ReadAt(raw, sidecarHeaderBytes+loIdx*SidecarKeyBytes)
	if got != len(raw) {
		if rerr == nil {
			rerr = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("sidecar: short read of bucket %d (%d/%d bytes): %w", bucketIdx, got, len(raw), rerr)
	}
	out := make([]uint64, cnt)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(raw[i*SidecarKeyBytes:])
	}
	return out, nil
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

// 32-byte canonical header w/ given keyCount
func makeSidecarHeader(keyCount uint64) [sidecarHeaderBytes]byte {
	var h [sidecarHeaderBytes]byte
	copy(h[0:4], sidecarMagic)
	binary.LittleEndian.PutUint16(h[4:6], sidecarFormatVer)
	binary.LittleEndian.PutUint16(h[6:8], sidecarHashAlgoXX)
	binary.LittleEndian.PutUint64(h[8:16], keyCount)
	binary.LittleEndian.PutUint64(h[16:24], parserVersion)
	// bytes 24..31 reserved (zero)
	return h
}
