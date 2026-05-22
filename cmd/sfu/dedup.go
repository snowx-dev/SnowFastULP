package main

import (
	"bufio"
	"bytes"
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
	// 1:1 w/ bucketPaths. entry i = dest_keys/bucket_NNNN.bin for bucket i
	// (phase 0 of -od), or "" if no dest keys. nil disables dest dedup.
	destBucketPaths []string
	workers         int
	keepBuckets     bool // debug aid
}

// one bucket of work + optional dest counterpart
type dedupJob struct {
	bucketIdx int
	inputPath string
	destPath  string // "" if no dest dedup
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
	if cfg.destBucketPaths != nil && len(cfg.destBucketPaths) != len(cfg.bucketPaths) {
		return 0, fmt.Errorf("destBucketPaths len %d != bucketPaths len %d",
			len(cfg.destBucketPaths), len(cfg.bucketPaths))
	}

	jobCh := make(chan dedupJob, min(64, len(cfg.bucketPaths)+1))
	errCh := make(chan error, cfg.workers)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runDedupWorker(ctx, jobCh, sink, cfg.keepBuckets, m); err != nil {
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
			dest := ""
			if cfg.destBucketPaths != nil {
				dest = cfg.destBucketPaths[i]
			}
			select {
			case jobCh <- dedupJob{bucketIdx: i, inputPath: p, destPath: dest}:
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

func runDedupWorker(ctx context.Context, jobCh <-chan dedupJob, sink lineSink, keepBuckets bool, m *metrics) error {
	if m != nil {
		m.activeWorkers.Add(1)
		defer m.activeWorkers.Add(-1)
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
				m.busyWorkers.Add(1)
			}
			err := dedupBucket(ws, j.inputPath, j.destPath, sink, m)
			if m != nil {
				m.busyWorkers.Add(-1)
				if err == nil {
					m.bucketsDone.Add(1)
				}
			}
			if err != nil {
				return err
			}
			if !keepBuckets {
				_ = os.Remove(j.inputPath)
				if j.destPath != "" {
					_ = os.Remove(j.destPath)
				}
			}
		}
	}
}

// dedups one bucket into sink. local batch flushes ~once per MiB so workers
// dont thrash the shared mutex. destPath (if set) is a phase-0 dest bucket
// loaded into a sortedUint64Set, every input hash tested first, hits go to
// linesSkippedByDest and skip output.
func dedupBucket(ws *dedupWorkState, inputPath, destPath string, sink lineSink, m *metrics) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	var destSet sortedUint64Set
	if destPath != "" {
		keys, lerr := loadDestBucketKeys(destPath)
		if lerr != nil {
			return fmt.Errorf("load dest bucket %s: %w", destPath, lerr)
		}
		destSet.Build(keys)
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

// 8 GiB cap = 1B keys/bucket, defence vs corrupt -temp-dir OOM
const maxDestBucketBytes = 8 << 30

// reads dest_keys/bucket_NNNN.bin (packed uint64 LE, no header)
func loadDestBucketKeys(path string) ([]uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxDestBucketBytes {
		return nil, fmt.Errorf("dest bucket %s: size %d exceeds cap %d", path, fi.Size(), int64(maxDestBucketBytes))
	}
	if fi.Size()%sidecarKeyBytes != 0 {
		return nil, fmt.Errorf("dest bucket %s size %d not multiple of %d", path, fi.Size(), sidecarKeyBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	n := len(data) / sidecarKeyBytes
	out := make([]uint64, n)
	for i := 0; i < n; i++ {
		out[i] = binary.LittleEndian.Uint64(data[i*sidecarKeyBytes:])
	}
	return out, nil
}
