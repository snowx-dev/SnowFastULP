package search

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

// TxtConfig holds plain-text search parameters.
type TxtConfig struct {
	Ctx      context.Context
	MatchAll bool
	Pattern  []byte
	// MultiMatcher, when non-nil, enables multi-pattern mode (see Config).
	MultiMatcher *MultiMatcher
	Workers      int
	Files        []string
	Metrics      *Metrics
	Hits         chan<- Hit
	ArchiveOrd   map[string]int
	OnFileError  func(path string, err error)
	OnFileDone   func(ord int)
	// MaxHitsPerFile caps the hits each plain-text file may contribute
	// (H-09): the same safety valve MaxHitsPerChunk provides for compressed
	// chunks. 0 = unbounded. When a file is truncated, OnFileCapped fires
	// exactly once with the number of hits actually emitted.
	MaxHitsPerFile int
	OnFileCapped   func(path string, emitted int)
}

// RunTxt searches plain .txt files via worker pool (no index/sidecar).
// caller sets headline counters (ChunksTotal etc), RunTxt updates only progress
func RunTxt(cfg TxtConfig) error {
	if !cfg.MatchAll && cfg.MultiMatcher == nil && len(cfg.Pattern) == 0 {
		return fmt.Errorf("empty pattern")
	}
	ctx := cfg.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	type task struct {
		path string
		ord  int
	}

	tasks := make(chan task, cfg.Workers*4)
	var wg sync.WaitGroup
	// per-file failures are collected thread-safely and returned at the end as
	// a PartialScanError, after all hits have been emitted.
	var failures FailureCollector
	recordErr := func(path string, err error) {
		failures.Add(path, -1, err)
		if cfg.OnFileError != nil {
			cfg.OnFileError(path, err)
		}
	}
	tracker := newArchiveTracker(cfg.Metrics, cfg.OnFileDone)
	for _, path := range cfg.Files {
		tracker.seed(cfg.ArchiveOrd[path], 1)
	}
	markFileDone := tracker.markDone
	bumpFile := tracker.bump

	worker := func() {
		defer wg.Done()
		for t := range tasks {
			// on cancel: drain silently. signaling done lets OrderedPrinter
			// flush partial results
			if ctx.Err() != nil {
				continue
			}

			var fileBytes int64
			if st, err := os.Stat(t.path); err == nil {
				fileBytes = st.Size()
			}

			f, err := os.Open(t.path)
			if err != nil {
				recordErr(t.path, err)
				bumpFile(fileBytes)
				markFileDone(t.ord)
				continue
			}

			// register w/ abort registry so SIGINT closes our fd
			var unreg func()
			if reg := fileabort.FromContext(ctx); reg != nil {
				unreg = reg.Register(f)
			}

			emitted := 0
			emit := func(h localHit) error {
				hit := Hit{
					ArchiveOrd: t.ord,
					Archive:    t.path,
					ChunkID:    0,
					Offset:     h.offset,
					Line:       h.line,
					PatternIdx: h.patternIdx,
				}
				select {
				case cfg.Hits <- hit:
					emitted++
					if cfg.Metrics != nil {
						cfg.Metrics.Hits.Add(1)
					}
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			capped, err := searchTxtFile(ctx, f, cfg.Pattern, cfg.MatchAll, cfg.MultiMatcher, cfg.Metrics, cfg.MaxHitsPerFile, emit)
			if capped && cfg.OnFileCapped != nil {
				cfg.OnFileCapped(t.path, emitted)
			}
			if unreg != nil {
				unreg()
			}
			_ = f.Close()

			if err != nil {
				recordErr(t.path, err)
			}
			bumpFile(fileBytes)
			markFileDone(t.ord)
		}
	}

	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go worker()
	}

	go func() {
		defer close(tasks)
		for _, path := range cfg.Files {
			ord := cfg.ArchiveOrd[path]
			select {
			case <-ctx.Done():
				return
			case tasks <- task{path: path, ord: ord}:
			}
		}
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

// searchTxtFile streams a plain-text file in decode-step reads, assembling
// complete lines across read seams via lineAssembler so a matched line is never
// truncated at a buffer boundary — no overlap window or on-disk backref needed,
// and pattern/match-all share one path.
//
// maxHits caps the hits this file may contribute (H-09): the same per-chunk
// safety valve the compressed path applies. 0 = unbounded. The returned bool
// reports a REAL truncation: the budget ran out while at least one further
// match existed, so the caller can surface incomplete results (OnFileCapped).
func searchTxtFile(ctx context.Context, f *os.File, pattern []byte, matchAll bool, matcher *MultiMatcher, metrics *Metrics, maxHits int, emit func(localHit) error) (bool, error) {
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

	buf := make([]byte, outWin)
	var src io.Reader = f
	if ctx != nil {
		src = &ctxReader{ctx: ctx, r: f}
	}

	absOff := int64(0)
	hits := make([]localHit, 0, 256)
	var asm lineAssembler
	capped := false
	emitted := 0
	// remaining per-file hit budget for the next feed/flush; <0 = unbounded
	// (MaxHitsPerFile 0). recomputed after every emit since emitted grows.
	rem := func() int {
		if maxHits > 0 {
			return maxHits - emitted
		}
		return -1
	}
	emitHits := func(batch []localHit) error {
		for _, h := range batch {
			if err := emit(h); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return capped, err
			}
		}
		readLen := len(buf)
		if readLen > defaultDecodeStep {
			readLen = defaultDecodeStep
		}
		n, err := src.Read(buf[:readLen])
		if n > 0 {
			if metrics != nil {
				metrics.BytesScanned.Add(int64(n))
			}
			var tr bool
			hits, tr = asm.feed(hits[:0], buf[:n], absOff, process, rem())
			// a leftover match beyond the cap means the file really truncated
			capped = capped || tr
			absOff += int64(n)
			emitted += len(hits)
			if err := emitHits(hits); err != nil {
				return capped, err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return capped, err
		}
	}

	var tr bool
	hits, tr = asm.flush(hits[:0], process, rem())
	capped = capped || tr
	emitted += len(hits)
	if err := emitHits(hits); err != nil {
		return capped, err
	}
	return capped, nil
}
