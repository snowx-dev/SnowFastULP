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

	"github.com/snowx-dev/SnowFastULP/internal/version"
)

const (
	maxRejectLineLen = 8192
	debugTick        = 30 * time.Second
)

// structured job log, -debug
type debugLog struct {
	mu    sync.Mutex
	w     *bufio.Writer
	f     *os.File
	start time.Time
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
	_, _ = fmt.Fprintf(d.w, format, args...)
}

// tagged event w/ elapsed prefix, flushes. lifecycle only, not hot loops
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

func (d *debugLog) writeHeader(bin string, started time.Time, argv, inputs []string, r *resolved) {
	if d == nil {
		return
	}
	d.Printf("=== SnowFastULP debug log ===\n")
	d.Printf("version: %s\n", version.String)
	d.Printf("GOOS/GOARCH: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	d.Printf("start UTC: %s\n", started.UTC().Format(time.RFC3339Nano))
	d.Printf("start local: %s\n", started.Format(time.RFC3339Nano))
	d.Printf("argv: %s\n", formatArgv(argv))
	d.Printf("--- Resolved inputs ---\n")
	d.Printf("fileCount: %d\n", len(inputs))
	d.Printf("totalInputBytes: %d\n", r.totalInputs)
	for _, p := range inputs {
		if st, err := os.Stat(p); err == nil {
			d.Printf("  %s size=%d\n", p, st.Size())
		} else {
			d.Printf("  %s stat_err=%v\n", p, err)
		}
	}
	d.Printf("--- Resolved pipeline ---\n")
	d.Printf("output: %s\n", r.cfg.Output)
	d.Printf("splitZst: %d", r.cfg.ZstChunkLines)
	if !r.cfg.Compress && r.cfg.ZstChunkLines > 0 {
		d.Printf(" (ignored: -zst not set)")
	}
	d.Printf("\n")
	if r.cfg.DeleteInputs {
		d.Printf("deleteInputs: true\n")
	}
	d.Printf("compress: %v no-uri: %v tempDir: %s\n", r.cfg.Compress, r.cfg.NoURI, r.tempDir)
	d.Printf("workers: %d dedupWorkers: %d bucketCount: %d useFastPath: %v chunkBytes: %d (%s)\n",
		r.workers, r.dedupWorkers, r.bucketCount, r.useFastPath, r.chunkBytes, humanBytes(r.chunkBytes))
	d.Flush()
}

// dumps mem/fast-path/bucket reasoning, called once after writeHeader
func (d *debugLog) logResolutionRationale(r *resolved) {
	if d == nil || r == nil {
		return
	}
	d.Printf("--- Resolution rationale ---\n")
	d.Printf("mem: total=%s available=%s cgroupLimit=%s effective=%s\n",
		humanBytesU(r.mem.total), humanBytesU(r.mem.available),
		humanBytesU(r.mem.cgroupLimit), humanBytesU(r.mem.effectiveAvailable()))

	avail := r.mem.effectiveAvailable()
	if avail > 0 {
		threshold := int64(avail / uint64(fastPathRAMRatio))
		minAvail := uint64(fastPathMinAvailMB) * 1024 * 1024
		d.Printf("fastPath: %v (totalInputs=%s, threshold=%s [available/%d], min-available=%s)\n",
			r.useFastPath, humanBytes(r.totalInputs), humanBytes(threshold),
			fastPathRAMRatio, humanBytesU(minAvail))
	} else {
		d.Printf("fastPath: %v (memInfo unavailable; fast path disabled)\n", r.useFastPath)
	}

	src := r.bucketsSource
	if src == "" {
		src = "unknown"
	}
	d.Printf("bucketCount: %d (%s)\n", r.bucketCount, src)

	d.Printf("sink: %s\n", sinkModeDescription(r))
	d.Flush()
}

// pure, no I/O
func sinkModeDescription(r *resolved) string {
	if !r.cfg.Compress {
		return "plain text"
	}
	if r.cfg.ZstChunkLines <= 0 {
		return "zst single archive"
	}
	return fmt.Sprintf("zst chunked (split every %d unique lines)", r.cfg.ZstChunkLines)
}

// uint64 wrapper, values > 1<<62 wrap to 0 B
func humanBytesU(n uint64) string {
	if n > 1<<62 {
		return "0 B"
	}
	return humanBytes(int64(n))
}

func (d *debugLog) printfPhase(tag string, elapsed time.Duration) {
	if d == nil {
		return
	}
	d.Printf("%s elapsed=%s\n", tag, elapsed.Truncate(time.Millisecond))
	d.Flush()
}

func (d *debugLog) logProgress(m *metrics) {
	if d == nil || m == nil {
		return
	}
	ph := m.phase.Load()
	d.Printf("[progress] phase=%d chunks=%d/%d buckets=%d/%d bucketBytes=%d/%d bytesRead=%d bytesWritten=%d linesRead=%d accepted=%d rejected=%d unique=%d\n",
		ph,
		m.chunksDone.Load(), m.chunksTotal.Load(),
		m.bucketsDone.Load(), m.bucketsTotal.Load(),
		m.bucketsBytesRead.Load(), m.bucketsBytesTotal.Load(),
		m.bytesRead.Load(), m.bytesWritten.Load(),
		m.linesRead.Load(), m.linesAccepted.Load(), m.linesRejected.Load(), m.linesUnique.Load())
	d.Flush()
}

// interim OD phase-0 state on each tick
func (d *debugLog) logODProgress(odm *odMetrics) {
	if d == nil || odm == nil {
		return
	}
	odPh := odm.phase.Load()
	if odPh == int32(odPhaseIdle) || odPh == int32(odPhaseDone) {
		return
	}
	d.Printf("[od progress] od_phase=%d archives_total=%d need_regen=%d regen_done=%d skipped=%d regen_bytes=%d/%d keys_loaded=%d/%d\n",
		odPh,
		odm.archivesTotal.Load(), odm.archivesNeedRegen.Load(),
		odm.archivesRegenedDone.Load(), odm.archivesSkipped.Load(),
		odm.regenBytesRead.Load(), odm.regenBytesTotal.Load(),
		odm.keysLoaded.Load(), odm.keysTotalEstimate.Load())
	d.Flush()
}

func (d *debugLog) logCompletion(m *metrics, wall time.Duration, r *resolved) {
	if d == nil || m == nil {
		return
	}
	d.Printf("--- Completion ---\n")
	d.Printf("ok wall=%s linesRead=%d linesAccepted=%d linesRejected=%d linesUnique=%d bytesWritten=%d\n",
		wall.Truncate(time.Millisecond),
		m.linesRead.Load(), m.linesAccepted.Load(), m.linesRejected.Load(),
		m.linesUnique.Load(), m.bytesWritten.Load())
	if r != nil && len(r.OutputPaths) > 0 {
		d.Printf("outputPaths:\n")
		var totalDisk int64
		for _, p := range r.OutputPaths {
			if fi, err := os.Stat(p); err == nil {
				totalDisk += fi.Size()
				d.Printf("  %s (%s on disk)\n", p, humanBytes(fi.Size()))
			} else {
				d.Printf("  %s (stat_err=%v)\n", p, err)
			}
		}
		if len(r.OutputPaths) > 1 {
			d.Printf("outputTotalOnDisk: %s\n", humanBytes(totalDisk))
		}
	} else if r != nil {
		d.Printf("outputPaths: %s\n", r.cfg.Output)
	}
	d.Flush()
}

// signalled = ctrl-c/SIGTERM vs internal failure. always followed by Close
func (d *debugLog) logTermination(err error, signalled bool, wall time.Duration) {
	if d == nil {
		return
	}
	d.Printf("--- Termination ---\n")
	if signalled {
		d.Printf("interrupted by signal wall=%s err=%v\n", wall.Truncate(time.Millisecond), err)
	} else {
		d.Printf("ERROR wall=%s err=%v\n", wall.Truncate(time.Millisecond), err)
	}
	d.Flush()
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

// non-clashing path under cwd, _2, _3 ... on collision
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

// parse() rejects, -debug-reject
type rejectRecorder struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  *os.File
}

func newRejectRecorder(path string) (*rejectRecorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &rejectRecorder{
		f: f,
		w: bufio.NewWriterSize(f, 256*1024),
	}, nil
}

func (r *rejectRecorder) Record(absPath, posRef, raw string) {
	if r == nil {
		return
	}
	esc := escapeRejectRaw(raw)
	if len(esc) > maxRejectLineLen {
		suffix := "…[truncated]"
		esc = esc[:maxRejectLineLen-len(suffix)] + suffix
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = fmt.Fprintf(r.w, "%s\t%s\t%s\n", absPath, posRef, esc)
}

func (r *rejectRecorder) Close() error {
	if r == nil || r.f == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.w.Flush(); err != nil {
		_ = r.f.Close()
		r.f = nil
		return err
	}
	err := r.f.Close()
	r.f = nil
	return err
}

func escapeRejectRaw(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return s
}

// coarse progress on a ticker. odm may be nil
func startDebugProgress(ctx context.Context, d *debugLog, m *metrics, odm *odMetrics) (stop func()) {
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
				d.logODProgress(odm)
			}
		}
	}()
	return func() { close(done) }
}
