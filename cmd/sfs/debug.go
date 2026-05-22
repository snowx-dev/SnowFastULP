package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

const debugTick = 30 * time.Second

type progressSample struct {
	at           time.Time
	indexBytes   int64
	bytesScanned int64
	chunksDone   int64
}

type debugLog struct {
	mu           sync.Mutex
	w            *bufio.Writer
	f            *os.File
	start        time.Time
	lastProgress progressSample
	hasProgress  bool
}

func newDebugLog(path string) (*debugLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &debugLog{
		f:     f,
		w:     bufio.NewWriterSize(f, 64*1024),
		start: time.Now(),
	}, nil
}

func (d *debugLog) Printf(format string, args ...any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writef(format, args...)
}

// caller must hold d.mu
func (d *debugLog) writef(format string, args ...any) {
	_, _ = fmt.Fprintf(d.w, format, args...)
}

func (d *debugLog) Event(format string, args ...any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = fmt.Fprintf(d.w, "[event +%s] ", time.Since(d.start).Truncate(time.Millisecond))
	_, _ = fmt.Fprintf(d.w, format, args...)
	if !strings.HasSuffix(format, "\n") {
		_, _ = fmt.Fprintln(d.w)
	}
	_ = d.w.Flush()
}

func (d *debugLog) Flush() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.w.Flush()
}

func (d *debugLog) Close() error {
	if d == nil || d.f == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.w.Flush(); err != nil {
		_ = d.f.Close()
		d.f = nil
		return err
	}
	err := d.f.Close()
	d.f = nil
	return err
}

type debugRunInfo struct {
	root            string
	pattern         string
	patternLen      int
	workers         int
	outFile         string
	silent          bool
	clean           bool
	cwd             string
	gomaxprocs      int
	uiMode          string
	stderrTTY       bool
	txtMode         bool
	archives        []string
	indexBytesTotal int64
}

func (d *debugLog) writeHeader(bin string, started time.Time, argv []string, info debugRunInfo) {
	if d == nil {
		return
	}
	d.Printf("=== SnowFastSearch debug log ===\n")
	d.Printf("version: %s\n", version.String)
	d.Printf("GOOS/GOARCH: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	d.Printf("GOMAXPROCS: %d\n", info.gomaxprocs)
	d.Printf("cwd: %s\n", info.cwd)
	d.Printf("start UTC: %s\n", started.UTC().Format(time.RFC3339Nano))
	d.Printf("start local: %s\n", started.Format(time.RFC3339Nano))
	d.Printf("argv: %s\n", formatArgv(argv))
	d.Printf("--- Resolved search ---\n")
	d.Printf("root: %s\n", info.root)
	d.Printf("pattern: %q\n", info.pattern)
	d.Printf("patternLen: %d\n", info.patternLen)
	d.Printf("workers: %d\n", info.workers)
	d.Printf("output: %s\n", debugOutputDesc(info.outFile))
	d.Printf("silent: %v\n", info.silent)
	d.Printf("clean: %v\n", info.clean)
	d.Printf("uiMode: %s\n", info.uiMode)
	d.Printf("stderrTTY: %v\n", info.stderrTTY)
	d.Printf("txtMode: %v\n", info.txtMode)
	d.Printf("archives: %d\n", len(info.archives))
	d.Printf("indexBytesTotal: %d\n", info.indexBytesTotal)
	d.Printf("--- Archives ---\n")
	for _, p := range info.archives {
		if st, err := os.Stat(p); err == nil {
			d.Printf("  %s size=%d mtime=%s\n", p, st.Size(), st.ModTime().UTC().Format(time.RFC3339))
		} else {
			d.Printf("  %s stat_err=%v\n", p, err)
		}
	}
	d.Printf("binary: %s\n", bin)
	d.Flush()
}

func debugOutputDesc(outFile string) string {
	if outFile == "" {
		return "stdout"
	}
	return outFile
}

func (d *debugLog) logProgress(m *search.Metrics) {
	if d == nil || m == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writeProgressLine(m)
}

func (d *debugLog) writeProgressLine(m *search.Metrics) {
	phase := "INDEX"
	switch m.Phase.Load() {
	case search.PhaseSearch:
		phase = "SEARCH"
	case search.PhaseDone:
		phase = "DONE"
	}

	now := time.Now()
	idxBytes := m.IndexBytesDone.Load()
	scanned := m.BytesScanned.Load()
	chunksDone := m.ChunksDone.Load()

	var rateSuffix string
	if d.hasProgress {
		dt := now.Sub(d.lastProgress.at).Seconds()
		if dt > 0 {
			idxDelta := float64(idxBytes-d.lastProgress.indexBytes) / dt / (1 << 20)
			scanDelta := float64(scanned-d.lastProgress.bytesScanned) / dt / (1 << 20)
			chunkDelta := float64(chunksDone-d.lastProgress.chunksDone) / dt
			rateSuffix = fmt.Sprintf(" indexRate=%.2fMB/s scanRate=%.2fMB/s chunkRate=%.1f/s",
				idxDelta, scanDelta, chunkDelta)
		}
	}
	d.lastProgress = progressSample{
		at:           now,
		indexBytes:   idxBytes,
		bytesScanned: scanned,
		chunksDone:   chunksDone,
	}
	d.hasProgress = true

	_, _ = fmt.Fprintf(d.w, "[progress +%s] phase=%s archivesIndexed=%d/%d archivesSearched=%d/%d indexActive=%d indexBytes=%d/%d chunks=%d/%d hits=%d scanned=%d%s\n",
		time.Since(d.start).Truncate(time.Millisecond),
		phase,
		m.ArchivesIndexed.Load(), m.ArchivesTotal.Load(),
		m.ArchivesDone.Load(), m.ArchivesTotal.Load(),
		m.IndexArchivesActive.Load(),
		idxBytes, m.IndexBytesTotal.Load(),
		chunksDone, m.ChunksTotal.Load(),
		m.Hits.Load(), scanned,
		rateSuffix)
	_ = d.w.Flush()
}

func (d *debugLog) logIndexEvent(archive string, meta index.EnsureMeta, chunks int) {
	if d == nil {
		return
	}
	sidecarMod := "-"
	if !meta.SidecarMod.IsZero() {
		sidecarMod = meta.SidecarMod.UTC().Format(time.RFC3339)
	}
	d.Event("indexed archive=%s action=%s sidecar=%q chunks=%d stale=%v missing=%v archiveMod=%s sidecarMod=%s",
		filepath.Base(archive),
		meta.Action,
		meta.SidecarPath,
		chunks,
		meta.Stale,
		meta.Missing,
		meta.ArchiveMod.UTC().Format(time.RFC3339),
		sidecarMod,
	)
}

func (d *debugLog) logCompletion(m *search.Metrics, wall time.Duration, info debugRunInfo) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if m != nil {
		d.writeProgressLine(m)
	}
	d.writef("--- Completion ---\n")
	if m != nil {
		d.writef("ok wall=%s phase=%d hits=%d archivesIndexed=%d archivesSearched=%d chunksDone=%d bytesScanned=%d indexBytesDone=%d\n",
			wall.Truncate(time.Millisecond),
			m.Phase.Load(),
			m.Hits.Load(),
			m.ArchivesIndexed.Load(),
			m.ArchivesDone.Load(),
			m.ChunksDone.Load(),
			m.BytesScanned.Load(),
			m.IndexBytesDone.Load())
	} else {
		d.writef("ok wall=%s\n", wall.Truncate(time.Millisecond))
	}
	if info.outFile != "" {
		if fi, err := os.Stat(info.outFile); err == nil {
			d.writef("outputFile: %s size=%d\n", info.outFile, fi.Size())
		} else {
			d.writef("outputFile: %s stat_err=%v\n", info.outFile, err)
		}
	}
	_ = d.w.Flush()
}

func (d *debugLog) logTermination(err error, signalled bool, wall time.Duration, m *search.Metrics) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if m != nil {
		d.writeProgressLine(m)
	}
	d.writef("--- Termination ---\n")
	if signalled {
		d.writef("interrupted by signal wall=%s err=%v\n", wall.Truncate(time.Millisecond), err)
	} else {
		d.writef("ERROR wall=%s err=%v\n", wall.Truncate(time.Millisecond), err)
	}
	_ = d.w.Flush()
}

func formatArgv(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Quote(a))
	}
	return b.String()
}

func debugStamp(t time.Time) string {
	return t.UTC().Format("20060102") + "_" + t.UTC().Format("150405")
}

func debugArtifactPath(cwd, stem, ext, stamp string) (string, error) {
	for i := 0; i < 1000; i++ {
		name := stem + "-" + stamp + ext
		if i > 0 {
			name = fmt.Sprintf("%s-%s_%d%s", stem, stamp, i+1, ext)
		}
		p := filepath.Join(cwd, name)
		_, err := os.Stat(p)
		if os.IsNotExist(err) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate unique path for %s-*%s under %s", stem, ext, cwd)
}

func startDebugProgress(ctx context.Context, d *debugLog, m *search.Metrics) (stop func()) {
	if d == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(debugTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				d.logProgress(m)
			}
		}
	}()
	return func() { close(done) }
}
