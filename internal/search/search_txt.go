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
	Ctx         context.Context
	MatchAll    bool
	Pattern     []byte
	Workers     int
	Files       []string
	Metrics     *Metrics
	Hits        chan<- Hit
	ArchiveOrd  map[string]int
	OnFileError func(path string, err error)
	OnFileDone  func(ord int)
}

// RunTxt searches plain .txt files via worker pool (no index/sidecar).
// caller sets headline counters (ChunksTotal etc), RunTxt updates only progress
func RunTxt(cfg TxtConfig) error {
	if !cfg.MatchAll && len(cfg.Pattern) == 0 {
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
	remaining := make(map[int]int64)
	for _, path := range cfg.Files {
		ord := cfg.ArchiveOrd[path]
		remaining[ord] = 1
	}

	var doneMu sync.Mutex
	markFileDone := func(ord int) {
		doneMu.Lock()
		remaining[ord]--
		done := remaining[ord] == 0
		if done && cfg.Metrics != nil {
			cfg.Metrics.ArchivesDone.Add(1)
		}
		doneMu.Unlock()
		if done && cfg.OnFileDone != nil {
			cfg.OnFileDone(ord)
		}
	}
	bumpFile := func(fileBytes int64) {
		if cfg.Metrics != nil {
			cfg.Metrics.ChunksDone.Add(1)
			if fileBytes > 0 {
				cfg.Metrics.BytesChunkDone.Add(fileBytes)
			}
		}
	}

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
				if cfg.OnFileError != nil {
					cfg.OnFileError(t.path, err)
				}
				bumpFile(fileBytes)
				markFileDone(t.ord)
				continue
			}

			// register w/ abort registry so SIGINT closes our fd
			var unreg func()
			if reg := fileabort.FromContext(ctx); reg != nil {
				unreg = reg.Register(f)
			}

			localHits, err := searchTxtFile(ctx, f, cfg.Pattern, cfg.MatchAll, cfg.Metrics)
			if unreg != nil {
				unreg()
			}
			_ = f.Close()

			if err != nil && cfg.OnFileError != nil {
				cfg.OnFileError(t.path, err)
			}
			if err == nil {
				for _, h := range localHits {
					hit := Hit{
						ArchiveOrd: t.ord,
						Archive:    t.path,
						ChunkID:    0,
						Offset:     h.offset,
						Line:       h.line,
					}
					if ctx.Err() != nil {
						break
					}
					select {
					case cfg.Hits <- hit:
						if cfg.Metrics != nil {
							cfg.Metrics.Hits.Add(1)
						}
					case <-ctx.Done():
					}
				}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// searchTxtFile streams a plain-text file in decode-step reads, assembling
// complete lines across read seams via lineAssembler so a matched line is never
// truncated at a buffer boundary — no overlap window or on-disk backref needed,
// and pattern/match-all share one path.
func searchTxtFile(ctx context.Context, f *os.File, pattern []byte, matchAll bool, metrics *Metrics) ([]localHit, error) {
	var process processFn
	if matchAll {
		process = matchAllRegion
	} else {
		matcher := newPatternMatcher(pattern)
		process = patternRegion(&matcher)
	}

	buf := make([]byte, outWin)
	var src io.Reader = f
	if ctx != nil {
		src = &ctxReader{ctx: ctx, r: f}
	}

	absOff := int64(0)
	var hits []localHit
	var asm lineAssembler

	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return hits, err
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
			hits = asm.feed(hits, buf[:n], absOff, process)
			absOff += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return hits, err
		}
	}

	return asm.flush(hits, process), nil
}
