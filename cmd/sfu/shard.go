package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// bucket record LE: [u64 hash][u32 line_len][line bytes, no \n]
//
// phase 1 (shard): N parser goroutines split each input into chunkBytes
// chunks, parse() each line, append valid records to bucket file at hash mod B.

const (
	defaultChunkBytes       = 100 * 1024 * 1024
	defaultReadBufBytes     = 4 * 1024 * 1024
	bucketRecordHeaderBytes = 12

	// total RAM budget for bucket write buffers. bucketBufBytes(B) returns
	// totalBytes/B clamped to [floor, ceil] so write-buf footprint is
	// near-constant regardless of B
	bucketWriterBufTotalBytes = 256 * 1024 * 1024
	bucketWriterBufFloorBytes = 64 * 1024
	bucketWriterBufCeilBytes  = 1024 * 1024
)

// per-bucket buf size for B buckets, total ~bucketWriterBufTotalBytes
func bucketBufBytes(B int) int {
	if B <= 0 {
		return bucketWriterBufCeilBytes
	}
	n := bucketWriterBufTotalBytes / B
	if n > bucketWriterBufCeilBytes {
		return bucketWriterBufCeilBytes
	}
	if n < bucketWriterBufFloorBytes {
		return bucketWriterBufFloorBytes
	}
	return n
}

// [start, end) byte range of one input. readers seek to start, drop a
// partial first line if start > 0, stop after the line crossing end.
// enc/bomBytes come from a one-time head sniff. utf-16 = single whole-file
// job (stateful decoder, no safe mid-stream resume)
type chunkJob struct {
	path     string
	start    int64
	end      int64
	size     int64
	index    int64
	enc      fileEncoding
	bomBytes int // BOM len to discard when reading from offset 0
}

// slices inputs into chunkBytes-sized jobs in input order. utf-16 = one
// job per file. noEncodingSniff=true treats every file as UTF-8 no-BOM
func buildChunkJobs(inputs []string, chunkBytes int64, noEncodingSniff bool) ([]chunkJob, error) {
	if chunkBytes <= 0 {
		return nil, fmt.Errorf("chunkBytes must be > 0")
	}
	jobs := make([]chunkJob, 0, len(inputs))
	var idx int64
	for _, p := range inputs {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		size := info.Size()
		if size <= 0 {
			continue
		}
		enc, bomBytes := encUTF8, 0
		if !noEncodingSniff {
			var err error
			enc, bomBytes, err = sniffEncoding(p)
			if err != nil {
				return nil, err
			}
		}
		if enc == encUTF16LE || enc == encUTF16BE {
			jobs = append(jobs, chunkJob{
				path: p, start: 0, end: size, size: size, index: idx,
				enc: enc, bomBytes: bomBytes,
			})
			idx++
			continue
		}
		for start := int64(0); start < size; start += chunkBytes {
			end := start + chunkBytes
			if end > size {
				end = size
			}
			jobs = append(jobs, chunkJob{
				path: p, start: start, end: end, size: size, index: idx,
				enc: enc, bomBytes: bomBytes,
			})
			idx++
		}
	}
	return jobs, nil
}

// bufio.Writer + mutex so any reader can push records to the same bucket.
// 1 MiB write batches, mutex held only across one record append
type bucketWriter struct {
	mu   sync.Mutex
	bw   *bufio.Writer
	file *os.File
}

func newBucketWriter(path string, bufBytes int) (*bucketWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &bucketWriter{file: f, bw: bufio.NewWriterSize(f, bufBytes)}, nil
}

// appends one record, mutex held only across the append. `line` is copied
// by bufio.Write so caller can reuse the backing array immediately
func (w *bucketWriter) writeBytes(hash uint64, line []byte) error {
	var hdr [bucketRecordHeaderBytes]byte
	binary.LittleEndian.PutUint64(hdr[0:8], hash)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(line)))

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.bw.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.bw.Write(line); err != nil {
		return err
	}
	return nil
}

func (w *bucketWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		_ = w.file.Close()
		return err
	}
	return w.file.Close()
}

type shardConfig struct {
	inputs     []string
	jobs       []chunkJob // optional, shard builds from inputs+chunkBytes if nil
	tempDir    string
	bucketName func(i int) string // tests override, default "shard_%05d.bin"
	buckets    int
	workers    int
	chunkBytes int64
	bufBytes   int
	noURI      bool
	loose      bool
	reject     *rejectRecorder
}

type shardResult struct {
	bucketPaths []string
}

func defaultBucketName(i int) string {
	return fmt.Sprintf("shard_%05d.bin", i)
}

// phase 1: read inputs in parallel, parse + validate, write to bucket
// at xxhash64(host:login:password) mod B. blocks until all chunks consumed
// and all buckets flushed
func shard(ctx context.Context, cfg shardConfig, m *metrics) (*shardResult, error) {
	if cfg.buckets <= 0 || cfg.buckets > 1<<16 {
		return nil, fmt.Errorf("buckets must be in (0, 65536]")
	}
	if cfg.workers <= 0 {
		return nil, fmt.Errorf("workers must be > 0")
	}
	if cfg.chunkBytes <= 0 {
		cfg.chunkBytes = defaultChunkBytes
	}
	if cfg.bufBytes <= 0 {
		cfg.bufBytes = bucketBufBytes(cfg.buckets)
	}
	nameFn := cfg.bucketName
	if nameFn == nil {
		nameFn = defaultBucketName
	}

	var jobs []chunkJob
	var err error
	if cfg.jobs != nil {
		jobs = cfg.jobs
	} else {
		jobs, err = buildChunkJobs(cfg.inputs, cfg.chunkBytes, false)
		if err != nil {
			return nil, err
		}
		if m != nil {
			m.chunksTotal.Store(int64(len(jobs)))
		}
	}

	if err := os.MkdirAll(cfg.tempDir, 0o755); err != nil {
		return nil, err
	}

	writers := make([]*bucketWriter, cfg.buckets)
	paths := make([]string, cfg.buckets)
	for i := 0; i < cfg.buckets; i++ {
		paths[i] = filepath.Join(cfg.tempDir, nameFn(i))
		w, err := newBucketWriter(paths[i], cfg.bufBytes)
		if err != nil {
			closeAndRemoveBuckets(writers[:i], paths[:i])
			return nil, err
		}
		writers[i] = w
	}
	// publish bucketsTotal early so TUI + -debug [progress] report N/N
	// from phase 1 onwards instead of 0/0
	if m != nil {
		m.bucketsTotal.Store(int64(cfg.buckets))
	}

	jobCh := make(chan chunkJob, min(64, len(jobs)+1))
	errCh := make(chan error, cfg.workers)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < cfg.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runShardWorker(ctx, jobCh, writers, cfg.noURI, cfg.loose, cfg.reject, m); err != nil {
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
		for _, j := range jobs {
			select {
			case jobCh <- j:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	if e, ok := <-errCh; ok && e != nil {
		closeAndRemoveBuckets(writers, paths)
		return nil, e
	}

	for _, w := range writers {
		if err := w.close(); err != nil {
			return nil, err
		}
	}
	return &shardResult{bucketPaths: paths}, nil
}

func closeAndRemoveBuckets(writers []*bucketWriter, paths []string) {
	for _, w := range writers {
		if w != nil {
			_ = w.close()
		}
	}
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(p)
		}
	}
}

// per-goroutine reusable buffers. one bufio.Reader (Reset between chunks),
// one lineFormatter (output buf + xxhash digest)
type shardWorkState struct {
	reader *bufio.Reader
	fmt    *lineFormatter
}

func newShardWorkState() *shardWorkState {
	return &shardWorkState{
		reader: bufio.NewReaderSize(nil, defaultReadBufBytes),
		fmt:    newLineFormatter(),
	}
}

// pulls chunks off jobCh, scans + parses + appends. any err returns
// immediately, caller cancels ctx to drain peers
func runShardWorker(ctx context.Context, jobCh <-chan chunkJob, writers []*bucketWriter, noURI, loose bool, rr *rejectRecorder, m *metrics) error {
	mask := uint64(len(writers) - 1)
	usePow2 := mask != 0 && (uint64(len(writers))&(uint64(len(writers))-1)) == 0
	if m != nil {
		m.activeWorkers.Add(1)
		defer m.activeWorkers.Add(-1)
	}
	ws := newShardWorkState()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case job, ok := <-jobCh:
			if !ok {
				// closed ch may mean cancel, propagate so shard() doesnt
				// report success on a partial run
				return ctx.Err()
			}
			if m != nil {
				m.busyWorkers.Add(1)
			}
			err := scanChunk(ws, job, writers, usePow2, mask, noURI, loose, rr, m)
			if m != nil {
				m.busyWorkers.Add(-1)
				if err == nil {
					m.chunksDone.Add(1)
				}
			}
			if err != nil {
				return err
			}
		}
	}
}

// reads [job.start, job.end) of job.path, parses each complete line.
// memory-bounded: one bufio.Reader per worker reused via Reset,
// shared lineFormatter, no per-line allocs on success
func scanChunk(ws *shardWorkState, job chunkJob, writers []*bucketWriter, usePow2 bool, mask uint64, noURI, loose bool, rr *rejectRecorder, m *metrics) error {
	f, err := os.Open(job.path)
	if err != nil {
		return err
	}
	defer f.Close()

	absPath, aerr := filepath.Abs(job.path)
	if aerr != nil {
		absPath = job.path
	}

	// if prev chunk ended on '\n', job.start is a line boundary and we
	// own the first line. otherwise skip the partial first read
	skipPartial := false
	if job.start > 0 {
		var prev [1]byte
		n, perr := f.ReadAt(prev[:], job.start-1)
		if perr != nil || n != 1 {
			skipPartial = true
		} else {
			skipPartial = prev[0] != '\n'
		}
	}

	if _, err := f.Seek(job.start, io.SeekStart); err != nil {
		return err
	}

	// first-chunk BOM discard. utf-16 jobs are always whole-file so
	// chunks at offset > 0 are utf-8 and never see a BOM
	if job.start == 0 && job.bomBytes > 0 {
		if _, err := io.CopyN(io.Discard, f, int64(job.bomBytes)); err != nil {
			return err
		}
	}

	// utf-16: wrap w/ transform.Reader so bufio sees normal \n-terminated
	// utf-8. counter sits between file and decoder so bytesRead reflects
	// raw source bytes (decoded utf-8 is ~half utf-16 LE for ASCII)
	var src io.Reader = f
	var counter *countingReader
	if job.enc == encUTF16LE || job.enc == encUTF16BE {
		counter = &countingReader{r: f}
		src = wrapReader(counter, job.enc)
	}
	ws.reader.Reset(src)
	reader := ws.reader
	currentOffset := job.start

	for {
		lineStart := currentOffset
		var rawBefore int64
		if counter != nil {
			rawBefore = counter.n
		}
		line, consumed, tooLong, rerr := readBoundedLine(reader, maxInputLineBytes)
		if consumed > 0 {
			// utf-16: decoded line is shorter than raw source bytes.
			// counter delta is bursty (bufio refills in 4 MiB) but
			// totals over the file are exact
			rawConsumed := consumed
			if counter != nil {
				rawConsumed = counter.n - rawBefore
			}
			if m != nil {
				m.bytesRead.Add(rawConsumed)
			}
			endOffset := currentOffset + consumed

			if skipPartial {
				if tooLong || strings.HasSuffix(line, "\n") {
					skipPartial = false
				}
			} else {
				if tooLong {
					if m != nil {
						m.linesRead.Add(1)
						m.linesRejected.Add(1)
					}
					if rr != nil {
						rr.Record(absPath, strconv.FormatInt(lineStart, 10), "<line too long>")
					}
				} else if err := processLine(ws, line, writers, usePow2, mask, noURI, loose, m, absPath, lineStart, rr); err != nil {
					return err
				}
			}

			currentOffset = endOffset
			if currentOffset >= job.end && !skipPartial {
				return nil
			}
		}

		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// validates one line, routes record to bucket. all consumed lines bump
// counters incl rejects. posBytes formatted lazily in reject branch
func processLine(ws *shardWorkState, line string, writers []*bucketWriter, usePow2 bool, mask uint64, noURI, loose bool, m *metrics, srcAbs string, posBytes int64, rr *rejectRecorder) error {
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" {
		return nil
	}
	if m != nil {
		m.linesRead.Add(1)
	}
	host, url, login, password, ok := parseFor(trimmed, loose)
	if !ok {
		if m != nil {
			m.linesRejected.Add(1)
		}
		if rr != nil {
			rr.Record(srcAbs, strconv.FormatInt(posBytes, 10), trimmed)
		}
		return nil
	}
	out := ws.fmt.FormatRecord(host, url, login, password, noURI)
	h := ws.fmt.HashKey(host, login, password)
	var idx uint64
	if usePow2 {
		idx = h & mask
	} else {
		idx = h % uint64(len(writers))
	}
	if err := writers[idx].writeBytes(h, out); err != nil {
		return err
	}
	if m != nil {
		m.linesAccepted.Add(1)
		m.bytesShard.Add(int64(len(out)) + bucketRecordHeaderBytes)
	}
	return nil
}
