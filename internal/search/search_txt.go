package search

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/snowx-dev/SnowFastULP/internal/fileabort"
)

// max on-disk backref to recover lines starting before the search buffer.
// 64 KiB caps "reasonable line len", longer lines stay truncated
const maxTxtLineBackref = 64 * 1024

// TxtConfig holds plain-text search parameters.
type TxtConfig struct {
	Ctx         context.Context
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
	if len(cfg.Pattern) == 0 {
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

			localHits, err := searchTxtFile(ctx, f, cfg.Pattern, cfg.Metrics)
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

// sliding-window BMH on plain text. seam-straddling matches recovered via
// backref read from disk so emitted lines arent buffer-truncated
func searchTxtFile(ctx context.Context, f *os.File, pattern []byte, metrics *Metrics) ([]localHit, error) {
	matcher := newPatternMatcher(pattern)
	overlap := len(pattern) - 1
	if overlap < 0 {
		overlap = 0
	}

	buf := make([]byte, outWin+overlap)
	dst := buf[overlap:]
	var src io.Reader = f
	if ctx != nil {
		src = &ctxReader{ctx: ctx, r: f}
	}

	absOff := int64(0)
	first := true
	var hits []localHit

	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return hits, err
			}
		}
		readLen := len(dst)
		if readLen > defaultDecodeStep {
			readLen = defaultDecodeStep
		}
		n, err := src.Read(dst[:readLen])
		if n > 0 {
			if metrics != nil {
				metrics.BytesScanned.Add(int64(n))
			}
			var searchPtr []byte
			var searchLen int
			var base int64
			if first {
				searchPtr = dst[:n]
				searchLen = n
				base = absOff
			} else {
				searchPtr = buf[:overlap+n]
				searchLen = overlap + n
				base = absOff - int64(overlap)
			}
			hits = append(hits, findHitsTxt(searchPtr, searchLen, &matcher, base, f)...)

			absOff += int64(n)
			first = false

			if overlap > 0 {
				if n >= overlap {
					copy(buf, dst[n-overlap:n])
				} else {
					copy(buf, buf[n:overlap])
					copy(buf[overlap-n:], dst[:n])
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return hits, err
		}
	}

	return hits, nil
}

// like appendHits but uses extractLineWithBackref so seam matches arent truncated
func findHitsTxt(text []byte, textLen int, matcher *patternMatcher, baseOff int64, f *os.File) []localHit {
	patLen := len(matcher.pat)
	if patLen == 0 || textLen < patLen {
		return nil
	}
	var hits []localHit
	offset := 0
	for offset+patLen <= textLen {
		rel := matcher.find(text[offset:textLen])
		if rel < 0 {
			break
		}
		pos := offset + rel
		line := extractLineWithBackref(text, textLen, pos, baseOff, f)
		if line != "" {
			hits = append(hits, localHit{offset: baseOff + int64(pos), line: line})
		}
		offset = pos + 1
	}
	return hits
}

// returns full line at matchPos. if line starts at text[0] and we're past
// the file head, reads backward up to maxTxtLineBackref to grab the prefix
func extractLineWithBackref(text []byte, textLen, matchPos int, bufStartAbs int64, f *os.File) string {
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
	line := bytes.TrimRight(text[start:end], "\r")
	if start != 0 || bufStartAbs == 0 || f == nil {
		return string(line)
	}
	backStart := bufStartAbs - maxTxtLineBackref
	if backStart < 0 {
		backStart = 0
	}
	backLen := bufStartAbs - backStart
	if backLen <= 0 {
		return string(line)
	}
	back := make([]byte, backLen)
	n, err := f.ReadAt(back, backStart)
	if err != nil && err != io.EOF {
		return string(line)
	}
	back = back[:n]
	if i := bytes.LastIndexByte(back, '\n'); i >= 0 {
		back = back[i+1:]
	} else if backStart > 0 {
		// no newline in backref window, keep truncated line vs incomplete chunk
		return string(line)
	}
	full := make([]byte, 0, len(back)+len(line))
	full = append(full, back...)
	full = append(full, line...)
	return string(bytes.TrimRight(full, "\r"))
}
