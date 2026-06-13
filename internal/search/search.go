package search

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/output"

	"github.com/klauspost/compress/zstd"
)

// reuse per-chunk []localHit backing array across chunks, dense-hit queries
// skip log2(N) slice-growth allocs. caps preserved across Puts.
var hitsPool = sync.Pool{
	New: func() any {
		s := make([]localHit, 0, 256)
		return &s
	},
}

const (
	outWin = 1 << 20
	// max bytes per dec.Read inside searchChunk. 1 MiB matches zindex.cpp,
	// fits L2 on Skylake-X+/Zen 4+/Apple Silicon. older Broadwell/Haswell
	// (256 KiB L2) or Zen 2/3 (512 KiB L2) lose ~3%, tune via -decode-step
	defaultDecodeStep = 1 << 20
	minDecodeStep     = 4 << 10
)

// Hit is one pattern match.
type Hit struct {
	ArchiveOrd int
	Archive    string
	ChunkID    int
	Offset     int64
	Line       string
}

// Metrics tracks progress for the TUI.
type Metrics struct {
	Phase                atomic.Int32 // 0=index, 1=search, 2=done
	ArchivesTotal        atomic.Int64
	ArchivesIndexed      atomic.Int64
	ArchivesDone         atomic.Int64
	ChunksTotal          atomic.Int64
	ChunksDone           atomic.Int64
	BytesScanned         atomic.Int64
	BytesScannedTotal    atomic.Int64
	BytesChunkDone       atomic.Int64 // uncompressed bytes from finished chunks
	Hits                 atomic.Int64
	IndexBytesTotal      atomic.Int64
	IndexBytesDone       atomic.Int64
	IndexArchivesActive  atomic.Int64
	IndexFrameScanActive atomic.Int64
	IndexDecodeActive    atomic.Int64
	indexFocusMu         sync.Mutex
	indexFocusName       string
}

const (
	PhaseIndex  = 0
	PhaseSearch = 1
	PhaseDone   = 2
)

// Config holds search parameters.
type Config struct {
	Ctx context.Context
	// max bytes per dec.Read. 0 = default 1 MiB, clamped to [minDecodeStep, outWin]
	DecodeStep int
	// per-chunk hit cap. 0 = unbounded. safety valve vs pathological queries
	// (eg `:` on multi-GiB ULP). when hit, chunk truncates and OnChunkCapped fires
	MaxHitsPerChunk int
	Pattern         []byte
	Workers         int
	Archives        []string
	Sidecars        map[string]*index.Sidecar
	Metrics         *Metrics
	Hits            chan<- Hit
	ArchiveOrd      map[string]int
	OnChunkError    func(archive string, chunkID int, err error)
	OnArchiveDone   func(ord int)
	// fires at most once per capped chunk. nil = silent truncation
	OnChunkCapped func(archive string, chunkID int, emitted int)
}

// clamp to [minDecodeStep, outWin]
func resolveDecodeStep(req int) int {
	if req <= 0 {
		return defaultDecodeStep
	}
	if req < minDecodeStep {
		return minDecodeStep
	}
	if req > outWin {
		return outWin
	}
	return req
}

// Run searches all archives using a worker pool over chunks.
func Run(cfg Config) error {
	if len(cfg.Pattern) == 0 {
		return fmt.Errorf("empty pattern")
	}
	ctx := cfg.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.Metrics != nil {
		var chunks int64
		var scanBytes int64
		for _, sc := range cfg.Sidecars {
			chunks += int64(len(sc.Chunks))
			for _, ch := range sc.Chunks {
				scanBytes += ch.UncompressedEnd - ch.UncompressedStart
			}
		}
		cfg.Metrics.ChunksTotal.Store(chunks)
		cfg.Metrics.BytesScannedTotal.Store(scanBytes)
		cfg.Metrics.Phase.Store(PhaseSearch)
	}

	type task struct {
		archive    string
		archiveOrd int
		chunk      index.Chunk
	}

	decodeStep := resolveDecodeStep(cfg.DecodeStep)
	tasks := make(chan task, cfg.Workers*4)
	var wg sync.WaitGroup
	remaining := make(map[int]int64)
	for _, arch := range cfg.Archives {
		sc := cfg.Sidecars[arch]
		if sc == nil {
			continue
		}
		ord := cfg.ArchiveOrd[arch]
		remaining[ord] = int64(len(sc.Chunks))
	}

	var archiveDoneMu sync.Mutex
	markChunkDone := func(ord int) {
		archiveDoneMu.Lock()
		remaining[ord]--
		done := remaining[ord] == 0
		if done && cfg.Metrics != nil {
			cfg.Metrics.ArchivesDone.Add(1)
		}
		archiveDoneMu.Unlock()
		if done && cfg.OnArchiveDone != nil {
			cfg.OnArchiveDone(ord)
		}
	}
	bumpChunk := func(chunkBytes int64) {
		if cfg.Metrics != nil {
			cfg.Metrics.ChunksDone.Add(1)
			if chunkBytes > 0 {
				cfg.Metrics.BytesChunkDone.Add(chunkBytes)
			}
		}
	}

	// per-worker single-slot fileCache holds <=1 open archive. dispatcher
	// hands chunks of one archive contiguously, prev fd closes on archive
	// switch. caps open archive fds at len(workers), avoids EMFILE on
	// 200-archive runs w/ 256-fd ulimit
	type workerSlot struct {
		path  string
		file  *os.File
		unreg func()
	}
	closeSlot := func(s *workerSlot) {
		if s.file == nil {
			return
		}
		if s.unreg != nil {
			s.unreg()
		}
		_ = s.file.Close()
		s.path = ""
		s.file = nil
		s.unreg = nil
	}
	openSlot := func(s *workerSlot, path string) (*os.File, error) {
		if s.path == path && s.file != nil {
			return s.file, nil
		}
		closeSlot(s)
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		s.path = path
		s.file = f
		if reg := fileabort.FromContext(ctx); reg != nil {
			s.unreg = reg.Register(f)
		}
		return f, nil
	}

	worker := func() {
		defer wg.Done()
		slot := &workerSlot{}
		defer closeSlot(slot)

		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return
		}
		defer dec.Close()

		for t := range tasks {
			chunkBytes := t.chunk.UncompressedEnd - t.chunk.UncompressedStart
			// on cancel: drain silently, skip bumpChunk + markChunkDone.
			// signaling "done" would let OrderedPrinter flush partial results
			if ctx.Err() != nil {
				_ = chunkBytes
				continue
			}
			file, err := openSlot(slot, t.archive)
			if err != nil {
				if cfg.OnChunkError != nil {
					cfg.OnChunkError(t.archive, t.chunk.ChunkID, err)
				}
				bumpChunk(chunkBytes)
				markChunkDone(t.archiveOrd)
				continue
			}
			hitsP := hitsPool.Get().(*[]localHit)
			*hitsP = (*hitsP)[:0]
			// emit streams each decode-step's matches to the drain loop as the
			// chunk is scanned, instead of withholding them until the whole
			// (multi-GB) chunk finishes — keeps the live display + -l responsive.
			emit := func(batch []localHit) error {
				for i := range batch {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					select {
					case cfg.Hits <- Hit{
						ArchiveOrd: t.archiveOrd,
						Archive:    t.archive,
						ChunkID:    t.chunk.ChunkID,
						Offset:     batch[i].offset,
						Line:       batch[i].line,
					}:
						if cfg.Metrics != nil {
							cfg.Metrics.Hits.Add(1)
						}
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			emitted, capped, err := searchChunk(ctx, file, dec, t.chunk, cfg.Pattern, cfg.Metrics, *hitsP, decodeStep, cfg.MaxHitsPerChunk, emit)
			if err != nil {
				if cfg.OnChunkError != nil {
					cfg.OnChunkError(t.archive, t.chunk.ChunkID, err)
				}
			}
			if capped && cfg.OnChunkCapped != nil {
				cfg.OnChunkCapped(t.archive, t.chunk.ChunkID, emitted)
			}
			*hitsP = (*hitsP)[:0]
			hitsPool.Put(hitsP)
			bumpChunk(chunkBytes)
			markChunkDone(t.archiveOrd)
		}
	}

	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go worker()
	}

	go func() {
		defer close(tasks)
		for _, arch := range cfg.Archives {
			sc := cfg.Sidecars[arch]
			if sc == nil {
				continue
			}
			ord := cfg.ArchiveOrd[arch]
			for _, ch := range sc.Chunks {
				select {
				case <-ctx.Done():
					return
				case tasks <- task{archive: arch, archiveOrd: ord, chunk: ch}:
				}
			}
		}
	}()

	wg.Wait()
	if cfg.Metrics != nil {
		cfg.Metrics.Phase.Store(PhaseDone)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

type localHit struct {
	offset int64
	line   string
}

// searchChunk decodes the chunk in decodeStep reads and, after each read,
// flushes that step's matches via emit — so hits reach the caller continuously
// rather than only when the whole chunk finishes. scratch is a reusable hit
// buffer (caller-pooled); it's cleared after every flush. Returns the number of
// hits emitted, whether the per-chunk cap (maxHits) truncated the chunk, and
// any decode/emit error (hits found before an error are still emitted).
func searchChunk(ctx context.Context, f *os.File, dec *zstd.Decoder, chunk index.Chunk, pattern []byte, metrics *Metrics, scratch []localHit, decodeStep, maxHits int, emit func([]localHit) error) (int, bool, error) {
	matcher := newPatternMatcher(pattern)
	overlap := len(pattern) - 1
	if overlap < 0 {
		overlap = 0
	}

	buf := make([]byte, outWin+overlap)
	dst := buf[overlap:]
	section := io.NewSectionReader(f, chunk.CompressedOffset, chunk.CompressedSize)
	var src io.Reader = section
	if ctx != nil {
		src = &ctxReader{ctx: ctx, r: section}
	}
	dec.Reset(src)

	absOff := chunk.UncompressedStart
	first := true
	capped := false
	emitted := 0
	hits := scratch[:0]

	// flush this step's accumulated hits, truncating to the per-chunk cap.
	flush := func() error {
		if len(hits) == 0 {
			return nil
		}
		if maxHits > 0 && emitted+len(hits) > maxHits {
			hits = hits[:maxHits-emitted]
		}
		if len(hits) > 0 {
			if err := emit(hits); err != nil {
				return err
			}
			emitted += len(hits)
		}
		hits = hits[:0]
		return nil
	}

	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return emitted, capped, err
			}
		}
		readLen := len(dst)
		if readLen > decodeStep {
			readLen = decodeStep
		}
		nOut, err := dec.Read(dst[:readLen])
		if nOut > 0 {
			if metrics != nil {
				metrics.BytesScanned.Add(int64(nOut))
			}
			var searchPtr []byte
			var searchLen int
			var base int64
			if first {
				searchPtr = dst[:nOut]
				searchLen = nOut
				base = absOff
			} else {
				searchPtr = buf[:overlap+nOut]
				searchLen = overlap + nOut
				base = absOff - int64(overlap)
			}
			hits = appendHits(hits, searchPtr, searchLen, &matcher, base)

			absOff += int64(nOut)
			first = false

			// emit this step's hits so they reach the drain loop promptly
			if ferr := flush(); ferr != nil {
				return emitted, capped, ferr
			}
			// cap reached, stop decoding. one cap notification per chunk
			if maxHits > 0 && emitted >= maxHits {
				capped = true
				return emitted, capped, nil
			}

			if overlap > 0 {
				if nOut >= overlap {
					copy(buf, dst[nOut-overlap:nOut])
				} else {
					copy(buf, buf[nOut:overlap])
					copy(buf[overlap-nOut:], dst[:nOut])
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return emitted, capped, err
		}
	}

	return emitted, capped, nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// writes matches into caller-owned dst slice, lets worker reuse pooled
// backing across chunks, removes log2(N) slice-grow allocs per chunk
func appendHits(dst []localHit, text []byte, textLen int, matcher *patternMatcher, baseOff int64) []localHit {
	patLen := len(matcher.pat)
	if patLen == 0 || textLen < patLen {
		return dst
	}
	offset := 0
	for offset+patLen <= textLen {
		rel := matcher.find(text[offset:textLen])
		if rel < 0 {
			break
		}
		pos := offset + rel
		line := extractLine(text, textLen, pos)
		if line != "" {
			dst = append(dst, localHit{offset: baseOff + int64(pos), line: line})
		}
		offset = pos + 1
	}
	return dst
}

func extractLine(text []byte, textLen, matchPos int) string {
	if matchPos >= textLen {
		return ""
	}
	start := matchPos
	end := matchPos
	for start > 0 && text[start-1] != '\n' {
		start--
	}
	for end < textLen && text[end] != '\n' && text[end] != 0 {
		end++
	}
	// inline CRLF strip, cheaper than bytes.TrimRight on dense-hit path
	if end > start && text[end-1] == '\r' {
		end--
	}
	return string(text[start:end])
}

// OrderedPrinter writes hits in archive/chunk/offset order.
// hits for later archives buffer in `pending` until earlier finishes.
// per-archive grouping by design, vs zindex.cpp interleaved.
type OrderedPrinter struct {
	mu          sync.Mutex
	nextArchive int
	pending     map[int][]Hit
	archiveDone map[int]bool
	write       func(Hit) error
}

// NewOrderedPrinter returns a printer that calls write for each in-order hit.
func NewOrderedPrinter(write func(Hit) error) *OrderedPrinter {
	return &OrderedPrinter{
		pending:     make(map[int][]Hit),
		archiveDone: make(map[int]bool),
		write:       write,
	}
}

// Add buffers a hit, flushes if its archive is the next ready one.
func (p *OrderedPrinter) Add(h Hit) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending[h.ArchiveOrd] = append(p.pending[h.ArchiveOrd], h)
	return p.flushReady()
}

// MarkArchiveDone marks ord finished and flushes any ready archives.
func (p *OrderedPrinter) MarkArchiveDone(ord int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.archiveDone[ord] = true
	return p.flushReady()
}

func (p *OrderedPrinter) flushReady() error {
	for {
		ord := p.nextArchive
		if !p.archiveDone[ord] {
			return nil
		}
		hits := p.pending[ord]
		if len(hits) > 0 {
			sort.Slice(hits, func(i, j int) bool {
				if hits[i].ChunkID != hits[j].ChunkID {
					return hits[i].ChunkID < hits[j].ChunkID
				}
				return hits[i].Offset < hits[j].Offset
			})
			for _, h := range hits {
				if err := p.write(h); err != nil {
					return err
				}
			}
		}
		delete(p.pending, ord)
		p.nextArchive++
	}
}

// Writer formats hits to an io.Writer.
type Writer struct {
	w     *bufio.Writer
	clean bool
}

// NewWriter wraps w w/ a 1 MiB buffer. clean strips URL schemes per hit.
func NewWriter(w io.Writer, clean bool) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 1<<20), clean: clean}
}

// WriteHit writes a single hit line, optionally cleaned.
func (pw *Writer) WriteHit(h Hit) error {
	line := h.Line
	if pw.clean {
		line = output.CleanLine(line)
	}
	_, err := fmt.Fprintln(pw.w, line)
	return err
}

// Flush flushes the buffered writer.
func (pw *Writer) Flush() error {
	return pw.w.Flush()
}
