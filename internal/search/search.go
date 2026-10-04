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
	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
	"github.com/snowx-dev/SnowFastULP/internal/zstdlimits"

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
	// max bytes per dec.Read inside searchChunk. 1 MiB matches zindex.cpp;
	// tune via -decode-step only after profiling the target workload.
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
	// PatternIdx identifies which input pattern matched, for routing hits to
	// per-pattern output files in multi-pattern (-f) mode. Zero in single-pattern
	// and match-all modes (the first/only pattern), so existing callers ignore it.
	PatternIdx int
}

// Metrics tracks progress for the TUI.
type Metrics struct {
	Phase             atomic.Int32 // 0=index, 1=search, 2=done
	ArchivesTotal     atomic.Int64
	ArchivesIndexed   atomic.Int64
	ArchivesDone      atomic.Int64
	ChunksTotal       atomic.Int64
	ChunksDone        atomic.Int64
	BytesScanned      atomic.Int64
	BytesScannedTotal atomic.Int64
	BytesChunkDone    atomic.Int64 // uncompressed bytes from finished chunks
	Hits              atomic.Int64
	// ChunksCapped counts chunks truncated by MaxHitsPerChunk: the run
	// returned incomplete results, so callers map it to the shared exit-code
	// policy's partial-failure code (internal/exitcode).
	ChunksCapped         atomic.Int64
	IndexBytesTotal      atomic.Int64
	IndexBytesDone       atomic.Int64
	IndexArchivesActive  atomic.Int64
	IndexFrameScanActive atomic.Int64
	IndexDecodeActive    atomic.Int64
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
	MatchAll        bool // pattern "*" — emit every non-empty line
	Pattern         []byte
	// MultiMatcher, when non-nil, enables multi-pattern mode: each line is
	// matched against every pattern in one pass and hits carry PatternIdx.
	// When nil, the single Pattern above is used via a BMH matcher. Mutually
	// exclusive with MatchAll.
	MultiMatcher  *MultiMatcher
	Workers       int
	Archives      []string
	Sidecars      map[string]*index.Sidecar
	Metrics       *Metrics
	Hits          chan<- Hit
	ArchiveOrd    map[string]int
	OnChunkError  func(archive string, chunkID int, err error)
	OnArchiveDone func(ord int)
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
	if !cfg.MatchAll && cfg.MultiMatcher == nil && len(cfg.Pattern) == 0 {
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
		lastChunk  bool         // H-10: ends the archive → may flush the trailing partial line
		init       lineAsmState // H-10: previous contiguous chunk's final assembler state
	}

	decodeStep := resolveDecodeStep(cfg.DecodeStep)
	tasks := make(chan task, cfg.Workers*4)
	var wg sync.WaitGroup
	// per-file/per-chunk failures are collected thread-safely and returned at
	// the end as a PartialScanError, after all hits have been emitted.
	var failures FailureCollector
	recordErr := func(archive string, chunkID int, err error) {
		failures.Add(archive, chunkID, err)
		if cfg.OnChunkError != nil {
			cfg.OnChunkError(archive, chunkID, err)
		}
	}
	handoffs := make(map[string]chan lineAsmState, len(cfg.Archives))
	var senders sync.WaitGroup
	tracker := newArchiveTracker(cfg.Metrics, cfg.OnArchiveDone)
	for _, arch := range cfg.Archives {
		if cfg.Sidecars[arch] != nil {
			handoffs[arch] = make(chan lineAsmState, 1)
		}
	}
	for _, arch := range cfg.Archives {
		sc := cfg.Sidecars[arch]
		if sc == nil {
			continue
		}
		tracker.seed(cfg.ArchiveOrd[arch], int64(len(sc.Chunks)))
	}
	markChunkDone := tracker.markDone
	bumpChunk := tracker.bump
	// H-10: every dispatched chunk publishes its final assembler state (or a
	// zero state after a failed chunk) so the archive's sender can dispatch
	// the next chunk. Buffered cap 1, so this never blocks a worker.
	publish := func(archive string, st lineAsmState) {
		select {
		case handoffs[archive] <- st:
		case <-ctx.Done():
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

		decOpts := []zstd.DOption{zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(zstdlimits.MaxDecoderWindow)}
		if ids, contents := zstdframe.RegisteredDecoderDicts(); len(ids) > 0 {
			for i := range ids {
				decOpts = append(decOpts, zstd.WithDecoderDictRaw(ids[i], contents[i]))
			}
		}
		dec, err := zstd.NewReader(nil, decOpts...)
		if err != nil {
			// Worker-setup failure (not tied to a chunk): surface it via the
			// sentinel archive="" / chunkID=-1 so callers can log it instead of
			// dying silently with zero output. It is also a partial-scan
			// failure: every chunk this worker would have handled is unscanned.
			recordErr("", -1, fmt.Errorf("zstd reader init: %w", err))
			return
		}
		defer dec.Close()

		// One decode window per worker, reused for every chunk it handles —
		// a per-chunk make([]byte, outWin) would add a 1 MiB alloc + GC
		// scan per chunk on multi-GB scans.
		buf := make([]byte, outWin)

		for t := range tasks {
			// on cancel: drain silently, skip bumpChunk + markChunkDone.
			// signaling "done" would let OrderedPrinter flush partial results
			if ctx.Err() != nil {
				continue
			}
			chunkBytes := t.chunk.UncompressedEnd - t.chunk.UncompressedStart
			file, err := openSlot(slot, t.archive)
			if err != nil {
				recordErr(t.archive, t.chunk.ChunkID, err)
				bumpChunk(chunkBytes)
				markChunkDone(t.archiveOrd)
				publish(t.archive, lineAsmState{})
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
						PatternIdx: batch[i].patternIdx,
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
			emitted, capped, finalState, err := searchChunk(ctx, file, dec, t.chunk, cfg.Pattern, cfg.MatchAll, cfg.MultiMatcher, cfg.Metrics, *hitsP, buf, decodeStep, cfg.MaxHitsPerChunk, emit, t.init, t.lastChunk)
			if err != nil {
				// Explain makes a rejected long-window frame actionable
				// (cap + remediation) in the user-facing partial-scan error;
				// other errors pass through unchanged.
				recordErr(t.archive, t.chunk.ChunkID, zstdlimits.Explain(err))
				// a failed chunk leaves no trustworthy carry: the next chunk
				// of this archive restarts line assembly from its own start
				finalState = lineAsmState{}
			}
			publish(t.archive, finalState)
			if capped && cfg.OnChunkCapped != nil {
				cfg.OnChunkCapped(t.archive, t.chunk.ChunkID, emitted)
			}
			// drop line-string references so the pooled backing array does
			// not keep discarded/emitted hit strings alive across chunks
			clear((*hitsP)[:cap(*hitsP)])
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

	// H-10: chunks of one archive may land on different workers in arbitrary
	// order, so chunk k+1 must not be dispatched until chunk k has published
	// its final line-assembler state. Each archive gets a buffered (cap 1)
	// handoff channel: the worker finishing chunk k drops the state into it
	// and immediately returns to the task pool (no head-of-line blocking),
	// while the archive's sender goroutine consumes the state and dispatches
	// chunk k+1. Handoff is per-archive — archives are independent streams.
	for _, arch := range cfg.Archives {
		sc := cfg.Sidecars[arch]
		if sc == nil {
			continue
		}
		senders.Add(1)
		go func(arch string, ord int, sc *index.Sidecar, h chan lineAsmState) {
			defer senders.Done()
			var init lineAsmState
			for k, ch := range sc.Chunks {
				if k > 0 {
					// chunk k runs only after chunk k-1 published its state
					select {
					case s := <-h:
						init = s
					case <-ctx.Done():
						return
					}
				}
				select {
				case <-ctx.Done():
					return
				case tasks <- task{
					archive:    arch,
					archiveOrd: ord,
					chunk:      ch,
					lastChunk:  k == len(sc.Chunks)-1,
					init:       init,
				}:
				}
			}
		}(arch, cfg.ArchiveOrd[arch], sc, handoffs[arch])
	}
	go func() {
		senders.Wait()
		close(tasks)
	}()

	wg.Wait()
	if cfg.Metrics != nil {
		cfg.Metrics.Phase.Store(PhaseDone)
	}
	// cancellation wins: an interrupted run reports interrupted, not partial
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := failures.Result(); err != nil {
		return err
	}
	return nil
}

type localHit struct {
	offset     int64
	line       string
	patternIdx int // which input pattern matched; 0 for single-pattern / match-all
}

// decoder is the subset of *zstd.Decoder used by searchChunk. Defining it as
// an interface lets regression tests inject a stub that reports bytes and an
// error on the same read (the klauspost decoder is concrete).
type decoder interface {
	Reset(r io.Reader) error
	Read(p []byte) (int, error)
}

// searchChunk decodes the chunk in decodeStep reads and, after each read,
// flushes that step's matches via emit — so hits reach the caller continuously
// rather than only when the whole (multi-GB) chunk finishes. A lineAssembler stitches
// complete lines across read seams, so a matched line is never truncated at a
// decode-step boundary (and a pattern straddling a seam needs no overlap window).
// scratch is a reusable hit buffer (caller-pooled); it's cleared after every
// flush. buf is the worker's reusable decode window (outWin bytes); every chunk
// handled by the worker decodes into it, so no per-chunk window allocation.
// init (H-10) seeds the assembler with the previous contiguous chunk's final
// state, so lines spanning concatenated zstd frames survive the chunk
// boundary; lastChunk reports whether this chunk ends the archive — only then
// is the trailing partial line flushed as a line. Returns the number of hits
// emitted, whether the per-chunk cap (maxHits) truncated the chunk, the
// assembler's final state (valid only on a nil error; callers must discard it
// after a failed chunk), and any decode/emit error (hits found before an
// error are still emitted).
func searchChunk(ctx context.Context, f *os.File, dec decoder, chunk index.Chunk, pattern []byte, matchAll bool, matcher *MultiMatcher, metrics *Metrics, scratch []localHit, buf []byte, decodeStep, maxHits int, emit func([]localHit) error, init lineAsmState, lastChunk bool) (int, bool, lineAsmState, error) {
	var process processFn
	switch {
	case matchAll:
		process = matchAllRegion
	case matcher != nil:
		process = multiPatternRegion(matcher)
	default:
		pm := newPatternMatcher(pattern)
		process = patternRegion(&pm)
	}

	// worker passes a full window; re-slice defensively.
	buf = buf[:cap(buf)]
	section := io.NewSectionReader(f, chunk.CompressedOffset, chunk.CompressedSize)
	// H-08: track how much compressed input the decoder has pulled. When the
	// cap stops the scan early, this (plus the uncompressed position) decides
	// whether the chunk was read to its end or silently truncated.
	compressed := &countingReader{r: section}
	var src io.Reader = compressed
	if ctx != nil {
		src = &ctxReader{ctx: ctx, r: compressed}
	}
	dec.Reset(src)

	absOff := chunk.UncompressedStart
	capped := false
	emitted := 0
	hits := scratch[:0]
	var asm lineAssembler
	asm.restore(init)

	// remaining per-chunk hit budget for the next feed/flush; <0 = unlimited
	// (MaxHitsPerChunk 0). recomputed after every flush since emitted grows.
	rem := func() int {
		if maxHits > 0 {
			return maxHits - emitted
		}
		return -1
	}

	// flush this step's accumulated hits. Budget enforcement upstream means
	// truncation here is defensive only; discarded hits must not leave their
	// line strings reachable from the pooled backing array.
	// capped is set only on real truncation (emitted+len(hits) > maxHits), so a
	// chunk whose hits exactly fill the cap is not flagged as truncated.
	flush := func() error {
		if len(hits) == 0 {
			return nil
		}
		if maxHits > 0 && emitted+len(hits) > maxHits {
			trunc := maxHits - emitted
			clear(hits[trunc:])
			hits = hits[:trunc]
			capped = true
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
				return emitted, capped, asm.state(), err
			}
		}
		readLen := len(buf)
		if readLen > decodeStep {
			readLen = decodeStep
		}
		nOut, err := dec.Read(buf[:readLen])
		if nOut > 0 {
			if metrics != nil {
				metrics.BytesScanned.Add(int64(nOut))
			}
			var truncated bool
			hits, truncated = asm.feed(hits, buf[:nOut], absOff, process, rem())
			// the process functions stop matching at the cap; a leftover
			// match beyond it means the chunk really truncated
			if truncated {
				capped = true
			}
			absOff += int64(nOut)

			// emit this step's hits so they reach the drain loop promptly
			if ferr := flush(); ferr != nil {
				return emitted, capped, asm.state(), ferr
			}
			// cap reached, stop decoding. capped (if set) was flagged inside
			// flush on actual truncation, not merely on hitting the boundary.
			// A decoder error delivered on this same read must not be
			// swallowed just because the cap stopped us: hits are already
			// flushed above, so surface the error together with the cap
			// state (OnChunkCapped and OnChunkError both fire downstream).
			if maxHits > 0 && emitted >= maxHits {
				if err != nil && err != io.EOF {
					return emitted, capped, asm.state(), err
				}
				// H-08: hits exactly filling the cap is only a clean, complete
				// scan when the whole chunk was actually consumed. Stopping
				// here with compressed bytes still unread — or uncompressed
				// output the decoder has not returned — means the rest of the
				// chunk was never scanned: the results are incomplete, so the
				// chunk is classified as capped.
				if compressed.n < chunk.CompressedSize ||
					absOff < chunk.UncompressedStart+(chunk.UncompressedEnd-chunk.UncompressedStart) {
					capped = true
				}
				return emitted, capped, asm.state(), nil
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return emitted, capped, asm.state(), err
		}
	}

	// Finalize the trailing line (no terminating newline) — but only when
	// this chunk ends the archive (H-10). A frame boundary inside a
	// concatenated zstd stream is mid-line: the partial line rides to the
	// next contiguous chunk via the returned state instead of being flushed
	// as a truncated line.
	var truncated bool
	if lastChunk {
		hits, truncated = asm.flush(hits, process, rem())
		if truncated {
			capped = true
		}
		if ferr := flush(); ferr != nil {
			return emitted, capped, asm.state(), ferr
		}
	}
	return emitted, capped, asm.state(), nil
}

// countingReader counts compressed bytes pulled through it (H-08).
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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

// extractLine returns the complete line containing the match at matchPos. text
// is a region of whole '\n'-terminated lines (assembled by lineAssembler), so the
// surrounding newlines are always present — the line is never buffer-truncated.
func extractLine(text []byte, textLen, matchPos int) string {
	if matchPos >= textLen {
		return ""
	}
	start := matchPos
	end := matchPos
	for start > 0 && text[start-1] != '\n' {
		start--
	}
	for end < textLen && text[end] != '\n' {
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
		line = cleanLine(line)
	}
	_, err := fmt.Fprintln(pw.w, line)
	return err
}

// Flush flushes the buffered writer.
func (pw *Writer) Flush() error {
	return pw.w.Flush()
}
