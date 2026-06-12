package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"
)

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
type outputSink struct {
	mu             sync.Mutex
	path           string
	bw             *bufio.Writer
	enc            *zstd.Encoder // nil unless compress=true
	f              *os.File
	frames         *zstFrameTracker
	writeSearchIdx bool
}

func newOutputSink(path string, compress bool, writeSearchIdx bool) (*outputSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	storePath := path
	if a, aerr := filepath.Abs(path); aerr == nil {
		storePath = filepath.Clean(a)
	} else {
		storePath = filepath.Clean(path)
	}
	s := &outputSink{f: f, path: storePath, writeSearchIdx: writeSearchIdx}
	if compress {
		// level 3 default, ~6-10x on ULP text, outpaces dedup writes
		enc, err := zstd.NewWriter(f)
		if err != nil {
			_ = f.Close()
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

// line + '\n'. fast path only, multi-worker callers should batch via writeBatch
func (s *outputSink) writeLine(line string, m *metrics) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.bw.WriteString(line)
	if err != nil {
		return err
	}
	if err := s.bw.WriteByte('\n'); err != nil {
		return err
	}
	written := int64(n) + 1
	if err := s.noteCompressedWrite(written); err != nil {
		return err
	}
	if m != nil {
		m.linesUnique.Add(1)
		m.bytesWritten.Add(written)
	}
	return nil
}

// pre-formatted block of N \n-terminated lines under one mutex acquire.
// caller must ensure every line ends with '\n'
func (s *outputSink) writeBatch(buf []byte, lineCount int, m *metrics) error {
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
			m.linesUnique.Add(int64(lineCount))
		}
		m.bytesWritten.Add(int64(len(buf)))
	}
	return nil
}

// flush bw, close enc, close f. idempotent so deferred safety-net close
// doesnt double-finalize
func (s *outputSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
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
	closeErr := s.f.Close()
	path := s.path
	var chunks []searchFrameChunk
	if s.frames != nil {
		chunks = s.frames.chunksCopy()
	}
	writeSearch := s.writeSearchIdx
	s.f = nil
	if flushErr != nil {
		return flushErr
	}
	if encErr != nil {
		return encErr
	}
	if closeErr != nil {
		return closeErr
	}
	if writeSearch && len(chunks) > 0 {
		if err := writeSearchSidecar(path, chunks); err != nil {
			return err
		}
	}
	return nil
}

type dedupConfig struct {
	bucketPaths []string
	// sorted (v3) library sidecars for -od. each bucket's dest keys are read
	// from these via top-bits range reads (sidecarBucketKeys). nil/empty
	// disables dest dedup. numBuckets is derived from len(bucketPaths).
	destSidecars []string
	odMetrics    *odMetrics // optional: ticks keysLoaded as buckets gather
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
func dedup(ctx context.Context, cfg dedupConfig, sink lineSink, m *metrics) (int64, error) {
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
		return m.linesUnique.Load(), nil
	}
	return 0, nil
}

// per-goroutine reusable buffers, reused across buckets
type dedupWorkState struct {
	reader   *bufio.Reader
	recBuf   []byte
	localBuf bytes.Buffer
}

func newDedupWorkState() *dedupWorkState {
	ws := &dedupWorkState{
		reader: bufio.NewReaderSize(nil, 4*1024*1024),
		recBuf: make([]byte, 0, 256),
	}
	ws.localBuf.Grow(dedupWorkerBatchBytes + 4096)
	return ws
}

func runDedupWorker(ctx context.Context, jobCh <-chan dedupJob, sink lineSink, keepBuckets bool, m *metrics, destSidecars []string, numBuckets int, odm *odMetrics) error {
	if m != nil {
		m.activeWorkers.Add(1)
		defer m.activeWorkers.Add(-1)
	}
	ws := newDedupWorkState()
	// one open handle per library sidecar, reused across every bucket this
	// worker processes (vs re-opening per bucket). closed when the worker exits.
	var destReaders map[string]*sidecarReader
	if len(destSidecars) > 0 {
		destReaders = make(map[string]*sidecarReader, len(destSidecars))
		defer func() {
			for _, sr := range destReaders {
				sr.close()
			}
		}()
	}
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
				m.busyWorkers.Add(1)
			}
			err := dedupBucket(ws, j.inputPath, j.bucketIdx, numBuckets, destSidecars, destReaders, sink, m, odm)
			if m != nil {
				m.busyWorkers.Add(-1)
				if err == nil {
					m.bucketsDone.Add(1)
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

// gatherDestBucketKeys reads bucket bucketIdx's keys from every library sidecar
// (top-bits range reads) and k-way merges the already-sorted per-sidecar ranges
// into one sorted, deduped slice. readers caches one open handle per sidecar
// (reused across buckets) so a sidecar is opened once per worker, not per
// bucket. Returns the merged keys plus the pre-merge gathered count (for the
// "reading index" progress, which is measured against per-sidecar key totals).
func gatherDestBucketKeys(readers map[string]*sidecarReader, destSidecars []string, bucketIdx, numBuckets int) (keys []uint64, gathered int, err error) {
	runs := make([][]uint64, 0, len(destSidecars))
	for _, path := range destSidecars {
		sr := readers[path]
		if sr == nil {
			if sr, err = openSidecarReader(path); err != nil {
				return nil, 0, fmt.Errorf("open dest sidecar %s: %w", filepath.Base(path), err)
			}
			readers[path] = sr
		}
		run, rerr := sr.bucketKeys(bucketIdx, numBuckets)
		if rerr != nil {
			return nil, 0, fmt.Errorf("read dest bucket %d from %s: %w", bucketIdx, filepath.Base(path), rerr)
		}
		if len(run) == 0 {
			continue
		}
		runs = append(runs, run)
		gathered += len(run)
		if int64(gathered) > maxDestBucketKeys {
			return nil, 0, fmt.Errorf("dest bucket %d exceeds %d keys; increase -buckets for this library size",
				bucketIdx, maxDestBucketKeys)
		}
	}
	return mergeSortedUnique(runs, gathered), gathered, nil
}

// u64RunHeap: min-heap of cursors into sorted []uint64 runs, for k-way merge.
type u64RunCursor struct {
	run []uint64
	pos int
}
type u64RunHeap []u64RunCursor

func (h u64RunHeap) Len() int           { return len(h) }
func (h u64RunHeap) Less(i, j int) bool { return h[i].run[h[i].pos] < h[j].run[h[j].pos] }
func (h u64RunHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *u64RunHeap) Push(x any)        { *h = append(*h, x.(u64RunCursor)) }
func (h *u64RunHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// mergeSortedUnique merges already-sorted runs into one ascending, deduped
// slice in O(N log R) — cheaper than concatenate-then-sort (O(N log N)) when the
// inputs are already sorted, as each per-sidecar bucket range is.
func mergeSortedUnique(runs [][]uint64, total int) []uint64 {
	h := make(u64RunHeap, 0, len(runs))
	for _, r := range runs {
		if len(r) > 0 { // empty runs would index run[0] in the heap's Less
			h = append(h, u64RunCursor{run: r})
		}
	}
	switch len(h) {
	case 0:
		return nil
	case 1:
		return h[0].run // single non-empty run: already sorted + unique
	}
	heap.Init(&h)
	out := make([]uint64, 0, total)
	have := false
	var last uint64
	for h.Len() > 0 {
		v := h[0].run[h[0].pos]
		if !have || v != last {
			out = append(out, v)
			last = v
			have = true
		}
		h[0].pos++
		if h[0].pos < len(h[0].run) {
			heap.Fix(&h, 0)
		} else {
			heap.Pop(&h)
		}
	}
	return out
}

// dedups one bucket into sink. local batch flushes ~once per MiB so workers
// dont thrash the shared mutex. for -od, the bucket's dest keys are gathered
// from the library sidecars' sorted ranges into a sortedUint64Set; every input
// hash is tested first, hits go to linesSkippedByDest and skip output.
func dedupBucket(ws *dedupWorkState, inputPath string, bucketIdx, numBuckets int, destSidecars []string, destReaders map[string]*sidecarReader, sink lineSink, m *metrics, odm *odMetrics) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var destSet sortedUint64Set
	if len(destSidecars) > 0 {
		keys, gathered, gerr := gatherDestBucketKeys(destReaders, destSidecars, bucketIdx, numBuckets)
		if gerr != nil {
			return gerr
		}
		if odm != nil {
			// "reading index" progress: keys pulled from the library this bucket
			// (pre-merge count, to match keysTotalEstimate's per-sidecar totals)
			odm.keysLoaded.Add(int64(gathered))
		}
		destSet.adoptSorted(keys) // already sorted + deduped by the k-way merge
	}

	ws.reader.Reset(f)
	br := ws.reader
	seen := make(map[uint64]struct{}, 1<<14)
	var hdr [bucketRecordHeaderBytes]byte

	ws.localBuf.Reset()
	var localLines int

	flush := func() error {
		if ws.localBuf.Len() == 0 {
			return nil
		}
		if err := sink.writeBatch(ws.localBuf.Bytes(), localLines, m); err != nil {
			return err
		}
		ws.localBuf.Reset()
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
			m.bucketsBytesRead.Add(int64(bucketRecordHeaderBytes))
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
			m.bucketsBytesRead.Add(int64(n))
		}
		// dest check first so library hits skip the seen map entirely
		if destSet.Len() > 0 && destSet.Contains(h) {
			if m != nil {
				m.linesSkippedByDest.Add(1)
			}
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		ws.localBuf.Write(ws.recBuf)
		ws.localBuf.WriteByte('\n')
		localLines++
		if ws.localBuf.Len() >= dedupWorkerBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}
