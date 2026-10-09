package ulpengine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"unsafe"

	"github.com/klauspost/compress/zstd"
	"github.com/snowx-dev/SnowFastULP/internal/atomicfs"
)

// unsafeString views b as a string without copying. The caller must not
// retain the result beyond b's lifetime.
func unsafeString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// phase 2: read each bucket file, hash-set dedup, append first-sights to
// the shared output. bucket files deleted after a clean drain.

const (
	defaultOutputBufBytes = 8 * 1024 * 1024
	dedupWorkerBatchBytes = 1 * 1024 * 1024
	maxRecordLineLen      = maxParsedLineLen // mirrors parse() guard
)

// final output writer, mutex-guarded for parallel dedup workers.
// when compress=true: bufio.Writer -> zstd.Encoder -> os.File. bufio sits
// on the uncompressed side so workers feed the encoder in ~MiB chunks.
// close order matters: flush bw, close enc (emits zstd EOF), close f.
//
// Publication is transactional: the archive is written to a unique
// same-directory temp file (".<base>.tmp-*") and both sidecar types stage
// their own temps. Nothing under the final name is touched until commit()
// renames the staged files into place, so a failed or cancelled run leaves
// any pre-existing output byte-for-byte intact. Lifecycle:
//
//	seal()   flush/close the archive + finish staged sidecars (publishes nothing)
//	commit() atomically rename staged archive + sidecars to their final names
//	abort()  close + remove only the unique staged temps
//
// All three are idempotent.
type outputSink struct {
	mu             sync.Mutex
	finalPath      string // abs, cleaned final archive path
	tempPath       string // unique staged archive temp in the same directory
	bw             *bufio.Writer
	enc            *zstd.Encoder // nil unless compress=true
	f              *os.File
	frames         *zstFrameTracker
	writeSearchIdx bool
	sidecar        *sidecarWriter // -od: hashes indexed during dedup, staged on seal
	deferDirSync   bool           // multipart: chunked sink batch-syncs dirs after the whole commit
	sealed         bool
	committed      bool
	aborted        bool
	sealErr        error  // first seal failure; surfaced by later seal/commit
	searchStaged   string // staged search-sidecar temp path ("" = none)
}

// parentDirs lists the immediate parent directories whose entries this sink's
// commit renames: the archive dir, the dedup sidecar dir, the search sidecar
// dir. The chunked sink dedupes these across parts and syncs each exactly
// once after the batch.
func (s *outputSink) parentDirs() []string {
	dirs := []string{filepath.Dir(s.finalPath)}
	if s.sidecar != nil {
		dirs = append(dirs, filepath.Dir(sidecarPathForArchive(s.finalPath)))
	}
	if s.searchStaged != "" {
		dirs = append(dirs, filepath.Dir(s.finalSearchSidecarPath()))
	}
	return dirs
}

// outputTempPattern builds the os.CreateTemp pattern for a staged archive:
// a hidden "<final base>.tmp-*" file in the archive's own directory.
func outputTempPattern(finalPath string) string {
	return "." + filepath.Base(finalPath) + ".tmp-*"
}

func absCleanPath(p string) string {
	c := filepath.Clean(p)
	if a, err := filepath.Abs(p); err == nil {
		c = filepath.Clean(a)
	}
	return c
}

func newOutputSink(path string, compress bool, writeSearchIdx bool) (*outputSink, error) {
	final := absCleanPath(path)
	f, err := os.CreateTemp(filepath.Dir(final), outputTempPattern(final))
	if err != nil {
		return nil, err
	}
	tempPath := f.Name()
	RegisterCleanupPath(tempPath)
	s := &outputSink{f: f, finalPath: final, tempPath: tempPath, writeSearchIdx: writeSearchIdx}
	if compress {
		// level 3 default, ~6-10x on ULP text, outpaces dedup writes
		enc, err := zstd.NewWriter(f)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tempPath)
			UnregisterCleanupPath(tempPath)
			return nil, err
		}
		s.enc = enc
		s.bw = bufio.NewWriterSize(enc, defaultOutputBufBytes)
		s.frames = newZstFrameTracker()
	} else {
		s.bw = bufio.NewWriterSize(f, defaultOutputBufBytes)
	}
	return s, nil
}

func newOutputSinkWithSidecar(path string, compress bool, writeSearchIdx bool) (*outputSink, error) {
	s, err := newOutputSink(path, compress, writeSearchIdx)
	if err != nil {
		return nil, err
	}
	sw, err := newSidecarWriter(path)
	if err != nil {
		_ = s.abort()
		return nil, err
	}
	s.sidecar = sw
	return s, nil
}

// retargetFinal re-points the staged archive (and its .idx sidecar writer) at
// a new final name. Valid only before seal: the chunked sink uses it to apply
// the _part1 suffix to part 1 once a second part opens. The staged temps are
// name-independent and stay untouched.
func (s *outputSink) retargetFinal(newFinal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || s.committed || s.aborted {
		return
	}
	s.finalPath = absCleanPath(newFinal)
	if s.sidecar != nil {
		s.sidecar.finalPath = sidecarPathForArchive(s.finalPath)
	}
}

// finalSearchSidecarPath returns the final search-sidecar path for this sink's
// current final archive name.
func (s *outputSink) finalSearchSidecarPath() string {
	if s.finalPath == "" {
		return ""
	}
	return searchSidecarPathForArchive(s.finalPath)
}

func (s *outputSink) noteCompressedWrite(n int64) error {
	if s.frames == nil || n <= 0 {
		return nil
	}
	s.frames.noteUncompressed(n)
	if s.frames.needsRotate() {
		return s.rotateZstFrameLocked()
	}
	return nil
}

func (s *outputSink) rotateZstFrameLocked() error {
	if s.enc == nil || s.frames == nil {
		return nil
	}
	if err := s.bw.Flush(); err != nil {
		return err
	}
	if err := s.enc.Close(); err != nil {
		return err
	}
	s.enc = nil
	if err := s.frames.recordFrame(s.f); err != nil {
		return err
	}
	enc, err := zstd.NewWriter(s.f)
	if err != nil {
		return err
	}
	s.enc = enc
	s.bw.Reset(s.enc)
	return nil
}

// pre-formatted block of N \n-terminated lines under one mutex acquire.
// caller must ensure every line ends with '\n'
func (s *outputSink) writeBatch(buf []byte, lineCount int, m *Metrics) error {
	if len(buf) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.bw.Write(buf); err != nil {
		return err
	}
	if err := s.noteCompressedWrite(int64(len(buf))); err != nil {
		return err
	}
	if m != nil {
		if lineCount > 0 {
			m.LinesUnique.Add(int64(lineCount))
		}
		m.BytesWritten.Add(int64(len(buf)))
	}
	return nil
}

// pre-formatted lines + parallel dedup hashes. hashes and lineCount must match.
// Per batch, archive bytes are written before sidecar keys, all under one mutex,
// so worker interleaving stays aligned with the part each line lands in. (A
// failed *run* discards both archive and sidecar temps together via abort —
// see pipeline cleanup.)
func (s *outputSink) writeBatchIndexed(buf []byte, hashes []uint64, lineCount int, m *Metrics) error {
	if len(buf) == 0 {
		return nil
	}
	if lineCount > 0 && len(hashes) != lineCount {
		return fmt.Errorf("writeBatchIndexed: %d hashes != %d lines", len(hashes), lineCount)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.bw.Write(buf); err != nil {
		return err
	}
	if err := s.noteCompressedWrite(int64(len(buf))); err != nil {
		return err
	}
	if s.sidecar != nil {
		for _, h := range hashes {
			if err := s.sidecar.WriteHash(h); err != nil {
				return err
			}
		}
	}
	if m != nil {
		if lineCount > 0 {
			m.LinesUnique.Add(int64(lineCount))
		}
		m.BytesWritten.Add(int64(len(buf)))
	}
	return nil
}

// sealLocked flushes the encoder/buffer, closes the archive temp, and stages
// the sidecars (finishing the .idx writer's temp and writing the staged search
// sidecar). It publishes nothing under any final name. On error the sink stays
// unsealed and abort() still removes every staged temp.
func (s *outputSink) sealLocked() error {
	if s.sealed || s.committed {
		return nil
	}
	if s.sealErr != nil {
		return s.sealErr
	}
	flushErr := s.bw.Flush()
	var encErr error
	if s.enc != nil {
		if s.frames != nil {
			encErr = s.enc.Close()
			s.enc = nil
			if encErr == nil {
				encErr = s.frames.recordFrame(s.f)
			}
		} else {
			encErr = s.enc.Close()
			s.enc = nil
		}
	}
	// durability: fsync the archive temp before close so the renamed final
	// never exposes unwritten bytes
	syncErr := durableSyncFile(s.tempPath)
	closeErr := s.f.Close()
	var chunks []searchFrameChunk
	if s.frames != nil {
		chunks = s.frames.chunksCopy()
	}
	writeSearch := s.writeSearchIdx
	failing := flushErr != nil || encErr != nil || syncErr != nil || closeErr != nil
	var sidecarErr, searchErr error
	if !failing {
		if s.sidecar != nil {
			// bind the sidecar to the staged temp: commit renames it to the
			// final archive unchanged, so size/mtime/inode/digest describe
			// both (same principle as the search sidecar identity).
			s.sidecar.SetArchiveSource(s.tempPath)
			if _, err := s.sidecar.finish(); err != nil {
				sidecarErr = fmt.Errorf("sidecar stage %s: %w", s.finalPath, err)
			}
		}
		if sidecarErr == nil && writeSearch && len(chunks) > 0 {
			// identity is taken from the staged temp: commit renames it to the
			// final path unchanged, so size/mtime/digest describe both.
			staged, err := stageSearchSidecar(s.finalPath, s.tempPath, chunks)
			if err != nil {
				searchErr = err
			} else {
				s.searchStaged = staged
			}
		}
	}
	s.f = nil
	for _, err := range []error{flushErr, encErr, syncErr, closeErr, sidecarErr, searchErr} {
		if err != nil {
			s.sealErr = err
			return err
		}
	}
	s.sealed = true
	return nil
}

// seal finalizes the staged archive bytes and staged sidecars. Idempotent;
// publishes nothing. Once sealing has failed it keeps reporting the same error
// instead of re-attempting half-flushed state. Seal after abort reports an
// error: the staged temps are gone, there is nothing left to seal.
func (s *outputSink) seal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return fmt.Errorf("output sink: seal after abort for %s", s.finalPath)
	}
	return s.sealLocked()
}

// commit publishes the staged archive and sidecars to their final names via
// atomic rename. Idempotent. The archive rename lands first; if a sidecar
// publish then fails, the archive stays committed and the error propagates
// (a later -od run regenerates a missing sidecar). Commit after abort reports
// an error: the staged temps are gone, nothing can be published.
func (s *outputSink) commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return nil
	}
	if s.aborted {
		return fmt.Errorf("output sink: commit after abort for %s", s.finalPath)
	}
	if err := s.sealLocked(); err != nil {
		return err
	}
	if err := atomicfs.Rename(s.tempPath, s.finalPath); err != nil {
		return fmt.Errorf("publish %s: %w", filepath.Base(s.finalPath), err)
	}
	// the staged temp is now the final archive, not a deletable scratch file
	UnregisterCleanupPath(s.tempPath)
	if !s.deferDirSync {
		// durability: flush the archive's directory entry after its final rename
		if err := durableSyncDir(filepath.Dir(s.finalPath)); err != nil {
			return fmt.Errorf("sync %s dir: %w", filepath.Base(s.finalPath), err)
		}
	}
	if s.sidecar != nil {
		if err := s.sidecar.publish(); err != nil {
			return fmt.Errorf("publish sidecar %s: %w", filepath.Base(s.finalPath), err)
		}
		if !s.deferDirSync {
			if err := durableSyncDir(filepath.Dir(sidecarPathForArchive(s.finalPath))); err != nil {
				return fmt.Errorf("sync %s sidecar dir: %w", filepath.Base(s.finalPath), err)
			}
		}
	}
	if s.searchStaged != "" {
		finalSearch := s.finalSearchSidecarPath()
		if err := publishSearchSidecar(s.searchStaged, s.finalPath); err != nil {
			return fmt.Errorf("publish search sidecar %s: %w", filepath.Base(finalSearch), err)
		}
		s.searchStaged = ""
		if !s.deferDirSync {
			if err := durableSyncDir(filepath.Dir(finalSearch)); err != nil {
				return fmt.Errorf("sync %s search sidecar dir: %w", filepath.Base(s.finalPath), err)
			}
		}
	}
	s.committed = true
	return nil
}

// abort closes the archive (if still open) and removes only this sink's unique
// staged temps: archive, .idx, and search sidecar. Final names are never
// touched. Idempotent; a no-op after commit and after abort.
func (s *outputSink) abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted || s.committed {
		return nil
	}
	s.aborted = true
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.tempPath != "" {
		keep(removeStagedTemp(s.tempPath))
	}
	if s.sidecar != nil {
		keep(s.sidecar.Abort())
	}
	if s.searchStaged != "" {
		keep(removeStagedTemp(s.searchStaged))
		s.searchStaged = ""
	}
	return firstErr
}

type dedupConfig struct {
	bucketPaths []string
	// sorted (v3) library sidecars for -od. each bucket's dest keys are read
	// from these via top-bits range reads (sidecarReader.bucketKeys). nil/empty
	// disables dest dedup. numBuckets is derived from len(bucketPaths).
	destSidecars []string
	odMetrics    *ODMetrics // optional: ticks keysLoaded as buckets gather
	workers      int
	keepBuckets  bool // debug aid
}

// one bucket of work
type dedupJob struct {
	bucketIdx int
	inputPath string
}

// phase 2 orchestrator. caller makes sink, calls dedup, closes sink.
// returns unique lines written.
func dedup(ctx context.Context, cfg dedupConfig, sink lineSink, m *Metrics) (int64, error) {
	if cfg.workers <= 0 {
		return 0, fmt.Errorf("workers must be > 0")
	}
	if len(cfg.bucketPaths) == 0 {
		return 0, nil
	}
	if sink == nil {
		return 0, fmt.Errorf("sink is nil")
	}

	numBuckets := len(cfg.bucketPaths)
	// fail fast: the sorted-sidecar range reads use a top-bits partition, which
	// requires a power-of-two bucket count. surface it here, not deep in a
	// per-bucket read, if bucket-count selection ever stops rounding to pow2.
	if len(cfg.destSidecars) > 0 && numBuckets&(numBuckets-1) != 0 {
		return 0, fmt.Errorf("dedup: -od needs a power-of-two bucket count, got %d", numBuckets)
	}
	jobCh := make(chan dedupJob, min(64, numBuckets+1))
	errCh := make(chan error, cfg.workers)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runDedupWorker(ctx, jobCh, sink, cfg.keepBuckets, m, cfg.destSidecars, numBuckets, cfg.odMetrics); err != nil {
				select {
				case errCh <- err:
					cancel()
				default:
				}
			}
		}()
	}

	go func() {
		defer close(jobCh)
		for i, p := range cfg.bucketPaths {
			select {
			case jobCh <- dedupJob{bucketIdx: i, inputPath: p}:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	if e, ok := <-errCh; ok && e != nil {
		return 0, e
	}
	if m != nil {
		return m.LinesUnique.Load(), nil
	}
	return 0, nil
}

// per-goroutine reusable buffers, reused across buckets
type dedupWorkState struct {
	reader      *bufio.Reader
	recBuf      []byte
	localBuf    bytes.Buffer
	localHashes []uint64
}

func newDedupWorkState() *dedupWorkState {
	ws := &dedupWorkState{
		reader: bufio.NewReaderSize(nil, 4*1024*1024),
		recBuf: make([]byte, 0, 256),
	}
	ws.localBuf.Grow(dedupWorkerBatchBytes + 4096)
	ws.localHashes = make([]uint64, 0, 4096)
	return ws
}

func runDedupWorker(ctx context.Context, jobCh <-chan dedupJob, sink lineSink, keepBuckets bool, m *Metrics, destSidecars []string, numBuckets int, odm *ODMetrics) error {
	if m != nil {
		m.ActiveWorkers.Add(1)
		defer m.ActiveWorkers.Add(-1)
	}
	ws := newDedupWorkState()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case j, ok := <-jobCh:
			if !ok {
				// closed ch could be natural drain or ctx cancel, surface the latter
				return ctx.Err()
			}
			if m != nil {
				m.BusyWorkers.Add(1)
			}
			err := dedupBucket(ctx, ws, j.inputPath, j.bucketIdx, numBuckets, destSidecars, sink, m, odm)
			if m != nil {
				m.BusyWorkers.Add(-1)
				if err == nil {
					m.BucketsDone.Add(1)
				}
			}
			if err != nil {
				return err
			}
			// input shard files are scratch; dest sidecars are the persistent
			// library and are never removed here.
			if !keepBuckets {
				_ = os.Remove(j.inputPath)
			}
		}
	}
}

// 1 GiB keys = 8 GiB/bucket. defence-in-depth vs a pathological bucket (e.g.
// a tiny user -buckets against a huge library, or a skewed distribution) that
// would otherwise gather an unbounded slice. B auto-sizing keeps real buckets
// far below this; hitting it means -buckets is too small for the library.
const maxDestBucketKeys = 1 << 30

// dedupRecordPollInterval bounds how often bucket scanning checks ctx, so the
// first Ctrl-C stops in-progress bucket work within one bounded batch of
// records instead of after an entire multi-GB bucket read.
const dedupRecordPollInterval = 4096

// test-only deterministic cancellation hooks (nil in production). Every ctx
// poll point in dedupBucket/gatherDestBucketKeys fires its hook before
// checking ctx.Err(), so tests cancel at an exact point instead of racing
// wall-clock sleeps.
var (
	dedupPollHook  func()
	gatherPollHook func()
)

// pollCtx fires the test-only cancel hook (when set) and reports ctx.Err();
// nil means keep going.
func pollCtx(ctx context.Context, hook func()) error {
	if hook != nil {
		hook()
	}
	return ctx.Err()
}

// gatherDestBucketKeys reads bucket bucketIdx's keys from every library
// sidecar (top-bits range reads) into ONE backing slice. Pass 1 opens each
// sidecar to compute its exact bucket range/count and enforce
// maxDestBucketKeys; pass 2 reopens each and decodes fixed-size byte blocks
// directly into the slice's assigned segments. Each sidecar is open for only
// one pass at a time and is CLOSED before the next is opened — a deliberate
// trade of reopen syscalls for a hard descriptor bound (workers × sidecars
// open simultaneously would hit EMFILE late in a big multi-part run). The
// slice is sorted + compacted in place, so peak storage is ~8 B/key plus
// bounded read scratch — no per-sidecar raw byte buffers, no per-sidecar
// decoded runs, no second merged output. Returns the compacted keys plus the
// pre-compact gathered count (for the "reading index" progress, which is
// measured against per-sidecar key totals). Cancellation stops the gather at
// each poll point and returns ctx.Err() WITHOUT deleting the persistent
// sidecars.
func gatherDestBucketKeys(ctx context.Context, destSidecars []string, bucketIdx, numBuckets int, odm *ODMetrics) (keys []uint64, gathered int, err error) {
	type keySeg struct{ lo, hi int64 }
	segs := make([]keySeg, len(destSidecars))
	total := 0
	for i, path := range destSidecars {
		if perr := pollCtx(ctx, gatherPollHook); perr != nil {
			return nil, 0, perr
		}
		sr, oerr := openSidecarReader(path)
		if oerr != nil {
			return nil, 0, fmt.Errorf("open dest sidecar %s: %w", filepath.Base(path), oerr)
		}
		lo, hi, rerr := sr.bucketRange(ctx, bucketIdx, numBuckets)
		sr.close()
		if rerr != nil {
			return nil, 0, fmt.Errorf("read dest bucket %d from %s: %w", bucketIdx, filepath.Base(path), rerr)
		}
		segs[i] = keySeg{lo: lo, hi: hi}
		total += int(hi - lo)
		if odm != nil {
			odm.KeysLoaded.Add(int64(hi - lo))
		}
		if int64(total) > maxDestBucketKeys {
			return nil, 0, fmt.Errorf("dest bucket %d exceeds %d keys; increase -buckets for this library size",
				bucketIdx, maxDestBucketKeys)
		}
	}

	keys = make([]uint64, total) // the single backing allocation
	off := 0
	for i, path := range destSidecars {
		n := int(segs[i].hi - segs[i].lo)
		if n == 0 {
			continue
		}
		if perr := pollCtx(ctx, gatherPollHook); perr != nil {
			return nil, 0, perr
		}
		sr, oerr := openSidecarReader(path)
		if oerr != nil {
			return nil, 0, fmt.Errorf("open dest sidecar %s: %w", filepath.Base(path), oerr)
		}
		derr := sr.decodeBucketRange(ctx, keys[off:off+n], segs[i].lo)
		sr.close()
		if derr != nil {
			return nil, 0, fmt.Errorf("read dest bucket %d from %s: %w", bucketIdx, filepath.Base(path), derr)
		}
		off += n
	}
	gathered = total

	// sort+compact is pure in-place work: poll at its call boundary instead
	if serr := ctx.Err(); serr != nil {
		return nil, 0, serr
	}
	return sortCompactUnique(keys), gathered, nil
}

// sortCompactUnique sorts keys ascending and dedups them IN PLACE — no second
// slice. The per-sidecar bucket ranges are concatenated segments, so a full
// pdqsort + compact is allocation-free and replaces the old k-way merge (which
// retained every per-sidecar run plus a second merged output).
func sortCompactUnique(keys []uint64) []uint64 {
	if len(keys) == 0 {
		return nil
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// dedupStrongIDLen is the byte length of the strong identity kept beside each
// 64-bit dedup key: SHA-256 condensed to 128 bits. A 64-bit hit alone is no
// longer proof of duplication (chosen xxHash64 collisions are constructible);
// the strong identity is compared before a record is dropped.
const dedupStrongIDLen = 16

// strongKeyID condenses b into the 128-bit identity stored beside a dedup key.
func strongKeyID(b []byte) [dedupStrongIDLen]byte {
	sum := sha256.Sum256(b)
	var id [dedupStrongIDLen]byte
	copy(id[:], sum[:dedupStrongIDLen])
	return id
}

// recordStrongID derives the strong identity of one bucket record. The
// identity must describe the dedup-key PREIMAGE (host, login, password), not
// the stored line: the line preserves the source URL, so two URL variants of
// one credential share a key but differ as lines and must still dedup.
// parseStored recovers the preimage — stored lines are FormatRecordStable
// output whose round-trip was verified at write time, so the parse cannot fail
// for a real record; defensively (a legacy/corrupt line) the raw line bytes
// stand in. line is only read here; callers reuse their record buffer
// immediately afterwards.
func recordStrongID(line []byte) [dedupStrongIDLen]byte {
	// Zero-copy view: parseStored only slices; the recovered fields are hashed
	// before the caller refills the record buffer, so nothing escapes.
	host, _, login, password, ok := parseStored(unsafeString(line))
	if ok {
		return strongKeyID(appendDedupKeyFields(nil, host, login, password))
	}
	return strongKeyID(line)
}

// dedups one bucket into sink. local batch flushes ~once per MiB so workers
// dont thrash the shared mutex. for -od, the bucket's dest keys are gathered
// from the library sidecars' sorted ranges into a sortedUint64Set; every input
// hash is tested first, hits go to linesSkippedByDest and skip output.
// Cancellation is polled before the bucket open, at bounded record intervals
// (dedupRecordPollInterval), and before every sink flush; on cancel it returns
// ctx.Err() WITHOUT deleting the bucket file or the persistent sidecars
// (bucket removal happens only after a clean drain in runDedupWorker).
func dedupBucket(ctx context.Context, ws *dedupWorkState, inputPath string, bucketIdx, numBuckets int, destSidecars []string, sink lineSink, m *Metrics, odm *ODMetrics) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var destSet sortedUint64Set
	if len(destSidecars) > 0 {
		keys, _, gerr := gatherDestBucketKeys(ctx, destSidecars, bucketIdx, numBuckets, odm)
		if gerr != nil {
			return gerr
		}
		destSet.adoptSorted(keys) // sorted ascending + deduped in place by sortCompactUnique
	}

	ws.reader.Reset(f)
	br := ws.reader
	// seen maps each 64-bit bucket key to the strong identity of its
	// first-seen record; a 64-bit hit only drops the record when the strong
	// identity matches too (see dedupStrongIDLen).
	seen := make(map[uint64][dedupStrongIDLen]byte, 1<<14)
	var hdr [bucketRecordHeaderBytes]byte

	ws.localBuf.Reset()
	var localLines int
	var recs int

	flush := func() error {
		if ws.localBuf.Len() == 0 {
			return nil
		}
		// poll before every sink flush: a canceled run must not keep writing
		if ferr := ctx.Err(); ferr != nil {
			return ferr
		}
		var err error
		if ils, ok := sink.(indexedLineSink); ok {
			err = ils.writeBatchIndexed(ws.localBuf.Bytes(), ws.localHashes, localLines, m)
		} else {
			err = sink.writeBatch(ws.localBuf.Bytes(), localLines, m)
		}
		if err != nil {
			return err
		}
		ws.localBuf.Reset()
		ws.localHashes = ws.localHashes[:0]
		localLines = 0
		return nil
	}

	for {
		_, rerr := io.ReadFull(br, hdr[:])
		if rerr == io.EOF {
			return flush()
		}
		if rerr != nil {
			if rerr == io.ErrUnexpectedEOF {
				return fmt.Errorf("truncated record header in %s", inputPath)
			}
			return rerr
		}
		if m != nil {
			m.BucketsBytesRead.Add(int64(bucketRecordHeaderBytes))
		}
		h := binary.LittleEndian.Uint64(hdr[0:8])
		n := binary.LittleEndian.Uint32(hdr[8:12])
		if n == 0 {
			continue
		}
		if n > maxRecordLineLen {
			// corrupt shard or version-incompat leftover, refuse multi-GB malloc
			return fmt.Errorf("record length %d exceeds max %d in %s", n, maxRecordLineLen, inputPath)
		}
		// reuse recBuf, grows to bucket's longest record
		if cap(ws.recBuf) < int(n) {
			ws.recBuf = make([]byte, n)
		} else {
			ws.recBuf = ws.recBuf[:n]
		}
		if _, err := io.ReadFull(br, ws.recBuf); err != nil {
			return fmt.Errorf("truncated record body in %s: %w", inputPath, err)
		}
		if m != nil {
			m.BucketsBytesRead.Add(int64(n))
		}
		// dest check first so library hits skip the seen map entirely.
		// Residual risk (review C-02): dest keys come from library sidecars,
		// which persist only the 64-bit key — no record lines or preimages —
		// so a chosen 64-bit collision against a library key can still
		// suppress a credential that the library already holds. Closing that
		// gap needs a sidecar format migration; in-run dedup (below) is
		// collision-checked instead.
		if destSet.Len() > 0 && destSet.Contains(h) {
			if m != nil {
				m.LinesSkippedByDest.Add(1)
			}
			continue
		}
		id := recordStrongID(ws.recBuf[:n])
		if prev, dup := seen[h]; dup {
			if prev == id {
				continue
			}
			// 64-bit collision between distinct credentials: keep both. The
			// stored identity stays the first-seen one, so a repeat of the
			// first record still dedups; a repeat of a later collision
			// partner would be re-emitted — an over-count, never a loss, and
			// it needs a constructible double collision.
		} else {
			seen[h] = id
		}
		ws.localHashes = append(ws.localHashes, h)
		ws.localBuf.Write(ws.recBuf)
		ws.localBuf.WriteByte('\n')
		localLines++
		recs++
		if recs%dedupRecordPollInterval == 0 {
			// bounded-interval cancellation poll
			if perr := pollCtx(ctx, dedupPollHook); perr != nil {
				return perr
			}
		}
		if ws.localBuf.Len() >= dedupWorkerBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}
