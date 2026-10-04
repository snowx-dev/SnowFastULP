package ulpengine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
type DebugLog struct {
	mu    sync.Mutex
	w     *bufio.Writer
	f     *os.File
	start time.Time
}

func NewDebugLog(path string) (*DebugLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return NewDebugLogFile(f), nil
}

// NewDebugLogFile wraps an already-opened file. Use CreateArtifactFile to
// obtain f when the path must not be raced between allocation and open.
func NewDebugLogFile(f *os.File) *DebugLog {
	return &DebugLog{
		f:     f,
		w:     bufio.NewWriterSize(f, 64*1024),
		start: time.Now(),
	}
}

func (d *DebugLog) Printf(format string, args ...any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = fmt.Fprintf(d.w, format, args...)
}

// tagged event w/ elapsed prefix, flushes. lifecycle only, not hot loops
func (d *DebugLog) Event(format string, args ...any) {
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

func (d *DebugLog) Flush() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.w.Flush()
}

func (d *DebugLog) Close() error {
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

func (d *DebugLog) WriteHeader(bin string, started time.Time, argv, inputs []string, r *Resolved) {
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
	d.Printf("totalInputBytes: %d\n", r.TotalInputs)
	for _, p := range inputs {
		if st, err := os.Stat(p); err == nil {
			d.Printf("  %s size=%d\n", p, st.Size())
		} else {
			d.Printf("  %s stat_err=%v\n", p, err)
		}
	}
	d.Printf("--- Resolved pipeline ---\n")
	d.Printf("output: %s\n", r.Cfg.Output)
	d.Printf("splitZst: %d", r.Cfg.ZstChunkLines)
	if !r.Cfg.Compress && r.Cfg.ZstChunkLines > 0 {
		d.Printf(" (ignored: -zst not set)")
	}
	d.Printf("\n")
	if r.Cfg.DeleteInputs {
		d.Printf("deleteInputs: true\n")
	}
	d.Printf("compress: %v no-uri: %v tempDir: %s\n", r.Cfg.Compress, r.Cfg.NoURI, r.TempDir)
	d.Printf("workers: %d dedupWorkers: %d bucketCount: %d useFastPath: %v chunkBytes: %d (%s)\n",
		r.Workers, r.DedupWorkers, r.BucketCount, r.UseFastPath, r.chunkBytes, humanBytes(r.chunkBytes))
	d.Flush()
}

// dumps mem/fast-path/bucket reasoning, called once after writeHeader
func (d *DebugLog) LogResolutionRationale(r *Resolved) {
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
			r.UseFastPath, humanBytes(r.TotalInputs), humanBytes(threshold),
			fastPathRAMRatio, humanBytesU(minAvail))
	} else {
		d.Printf("fastPath: %v (memInfo unavailable; fast path disabled)\n", r.UseFastPath)
	}

	src := r.bucketsSource
	if src == "" {
		src = "unknown"
	}
	d.Printf("bucketCount: %d (%s)\n", r.BucketCount, src)

	d.Printf("sink: %s\n", sinkModeDescription(r))
	d.Flush()
}

// pure, no I/O
func sinkModeDescription(r *Resolved) string {
	if !r.Cfg.Compress {
		return "plain text"
	}
	if r.Cfg.ZstChunkLines <= 0 {
		return "zst single archive"
	}
	return fmt.Sprintf("zst chunked (split every %d unique lines)", r.Cfg.ZstChunkLines)
}

// uint64 wrapper, values > 1<<62 wrap to 0 B
func humanBytesU(n uint64) string {
	if n > 1<<62 {
		return "0 B"
	}
	return humanBytes(int64(n))
}

func (d *DebugLog) printfPhase(tag string, elapsed time.Duration) {
	if d == nil {
		return
	}
	d.Printf("%s elapsed=%s\n", tag, elapsed.Truncate(time.Millisecond))
	d.Flush()
}

func (d *DebugLog) logProgress(m *Metrics) {
	if d == nil || m == nil {
		return
	}
	ph := m.Phase.Load()
	d.Printf("[progress] phase=%d chunks=%d/%d buckets=%d/%d bucketBytes=%d/%d bytesRead=%d bytesWritten=%d linesRead=%d accepted=%d rejected=%d unique=%d\n",
		ph,
		m.ChunksDone.Load(), m.ChunksTotal.Load(),
		m.BucketsDone.Load(), m.BucketsTotal.Load(),
		m.BucketsBytesRead.Load(), m.BucketsBytesTotal.Load(),
		m.BytesRead.Load(), m.BytesWritten.Load(),
		m.LinesRead.Load(), m.LinesAccepted.Load(), m.LinesRejected.Load(), m.LinesUnique.Load())
	d.Flush()
}

// interim OD phase-0 state on each tick
func (d *DebugLog) logODProgress(odm *ODMetrics) {
	if d == nil || odm == nil {
		return
	}
	odPh := odm.Phase.Load()
	if odPh == int32(odPhaseIdle) || odPh == int32(odPhaseDone) {
		return
	}
	d.Printf("[od progress] od_phase=%d archives_total=%d need_regen=%d regen_done=%d skipped=%d regen_bytes=%d/%d keys_loaded=%d/%d\n",
		odPh,
		odm.ArchivesTotal.Load(), odm.ArchivesNeedRegen.Load(),
		odm.ArchivesRegenedDone.Load(), odm.ArchivesSkipped.Load(),
		odm.RegenBytesRead.Load(), odm.RegenBytesTotal.Load(),
		odm.KeysLoaded.Load(), odm.KeysTotalEstimate.Load())
	d.Flush()
}

func (d *DebugLog) LogCompletion(m *Metrics, wall time.Duration, r *Resolved) {
	if d == nil || m == nil {
		return
	}
	d.Printf("--- Completion ---\n")
	d.Printf("ok wall=%s linesRead=%d linesAccepted=%d linesRejected=%d linesUnique=%d bytesWritten=%d\n",
		wall.Truncate(time.Millisecond),
		m.LinesRead.Load(), m.LinesAccepted.Load(), m.LinesRejected.Load(),
		m.LinesUnique.Load(), m.BytesWritten.Load())
	d.Printf("ambiguity total=%d path_or_password=%d port_or_login=%d\n", m.AmbiguityTotal.Load(), m.AmbiguityPathPassword.Load(), m.AmbiguityPortLogin.Load())
	if tl, ptl, mal, un := m.LinesTooLong.Load(), m.LinesPasswordTooLong.Load(), m.LinesMalformed.Load(), m.LinesUnrepresentable.Load(); tl != 0 || ptl != 0 || mal != 0 || un != 0 {
		d.Printf("rejectReasons tooLong=%d passwordTooLong=%d malformed=%d unrepresentable=%d\n", tl, ptl, mal, un)
	}
	if n := m.LinesUnrepresentable.Load(); n > 0 {
		d.Printf("droppedUnrepresentable=%d (parsed but no round-trippable line form; excluded to avoid regen stragglers)\n", n)
	}
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
		d.Printf("outputPaths: %s\n", r.Cfg.Output)
	}
	d.Flush()
}

// signalled = ctrl-c/SIGTERM vs internal failure. always followed by Close
func (d *DebugLog) LogTermination(err error, signalled bool, wall time.Duration) {
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

// CreateArtifactFile exclusively creates dir/base<ext> and returns the opened
// file plus its path; on EEXIST it retries with _2, _3 … inserted before the
// extension. The create lives inside the retry loop, so a path is only
// returned after this caller atomically created it — two processes racing on
// the same base get distinct files instead of both opening one O_TRUNC path.
// The first (no-collision) name is byte-identical to the historical
// prospective path: base<ext> with no suffix.
func CreateArtifactFile(dir, base, ext string, perm fs.FileMode) (*os.File, string, error) {
	for i := range 1000 {
		name := base + ext
		if i > 0 {
			name = fmt.Sprintf("%s_%d%s", base, i+1, ext)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err == nil {
			return f, p, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("could not allocate unique path for %s*%s under %s", base, ext, dir)
}

// parse() rejects, -debug-reject
type RejectRecorder struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  *os.File
}

func NewRejectRecorder(path string) (*RejectRecorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return NewRejectRecorderFile(f), nil
}

// NewRejectRecorderFile wraps an already-opened file; pair it with
// CreateArtifactFile when the path must not be raced.
func NewRejectRecorderFile(f *os.File) *RejectRecorder {
	return &RejectRecorder{
		f: f,
		w: bufio.NewWriterSize(f, 256*1024),
	}
}

// Record records a rejected line untagged: three tab-separated columns
// (path, position ref, escaped raw line).
func (r *RejectRecorder) Record(absPath, posRef, raw string) {
	r.write(absPath, posRef, "", raw)
}

// RecordTagged records a rejected line with a short reason tag (e.g.
// "password>64"): the tag prefixes the escaped raw field as "[tag] raw",
// keeping the three-column shape of the untagged rows.
func (r *RejectRecorder) RecordTagged(absPath, posRef, tag, raw string) {
	r.write(absPath, posRef, tag, raw)
}

func (r *RejectRecorder) write(absPath, posRef, tag, raw string) {
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
	if tag != "" {
		_, _ = fmt.Fprintf(r.w, "%s\t%s\t[%s] %s\n", absPath, posRef, tag, esc)
		return
	}
	_, _ = fmt.Fprintf(r.w, "%s\t%s\t%s\n", absPath, posRef, esc)
}

// Flush pushes buffered reject lines to disk without closing. Best-effort and
// silent by contract: process-exit seams call it because os.Exit skips the
// deferred Close, and a flush failure must never mask the real fatal message
// or alter the exit code of a dying process. Safe after Close — the write
// fails into the void, silently.
func (r *RejectRecorder) Flush() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.w.Flush()
}

func (r *RejectRecorder) Close() error {
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
func startDebugProgress(ctx context.Context, d *DebugLog, m *Metrics, odm *ODMetrics) (stop func()) {
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
