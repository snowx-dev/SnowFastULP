package ulpengine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/fdlimit"
	"github.com/snowx-dev/SnowFastULP/internal/pathident"
)

// user-facing knobs threaded into shard/dedup
type Config struct {
	Inputs        []string
	Output        string
	TempDir       string // shard temp, defaults to outDir
	Workers       int    // phase 1, 0=auto
	DedupWorkers  int    // phase 2, 0=auto
	Buckets       int    // 0=adaptive
	ChunkBytes    int64  // 0=defaultChunkBytes
	FastPathOff   bool
	Compress      bool
	ZstChunkLines int64 // 0=single .zst, N=split every N unique lines
	RunStarted    time.Time
	// shared per-run id, "<YYYYMMDD>_<runID>". empty in tests that
	// bypass main, chunkedZstdSink then falls back to date-only
	RunStamp        string
	DeleteInputs    bool
	NoURI           bool
	Loose           bool
	NoEncodingSniff bool // -no-encoding-sniff, forces UTF-8 path
	DestDedup       bool // -od
	DestDedupDir    string
	Debug           *DebugLog
	Reject          *RejectRecorder
}

// default -zst split granularity, lands ~1.2-1.8 GB compressed/part
// on typical ULP text. -split-zst 0 disables splitting
const defaultZstChunkLines int64 = 100_000_000

const (
	minBuckets            = 64
	maxBuckets            = 4096
	fastPathRAMRatio      = 8       // input < MemAvail/8 enables fast path
	fastPathMinAvailMB    = 1_024   // need 1 GiB MemAvail to try
	maxFastPathInputBytes = 1 << 30 // switch big jobs to worker/bucket path

	// FDs reserved for stdio, walks, current input, sink, runtime.
	// effective bucket cap = soft_limit - fdReserve, floored to pow2
	fdReserve = 32
)

// Resolved pipeline params after defaults filled. surfaced to TUI
type Resolved struct {
	Cfg            Config
	TotalInputs    int64
	InputFileCount int
	mem            memInfo
	UseFastPath    bool
	chunkBytes     int64
	BucketCount    int
	// how bucketCount was decided, eg "user (rounded up) (fd-clamped)"
	bucketsSource     string
	Workers           int
	DedupWorkers      int
	TempDir           string
	OutputPaths       []string
	DeletedInputPaths []string
	// live phase 0 / sidecar counters, populated when -od set
	OdMetrics *ODMetrics
	// immutable phase 0 outcome for end-of-run recap, nil w/o -od
	OdResult *ODResult
	// separate odMetrics for phaseIndex (own-output .idx pass),
	// kept distinct so phase 0 recap reflects library scan numbers
	OutputIdxMetrics *ODMetrics
}

// fills defaults, decides fast-path eligibility. no I/O beyond stat+meminfo
func Resolve(cfg Config) (*Resolved, error) {
	if len(cfg.Inputs) == 0 {
		return nil, fmt.Errorf("no input files")
	}
	if cfg.Output == "" {
		return nil, fmt.Errorf("output path is required")
	}

	total, err := totalInputBytes(cfg.Inputs)
	if err != nil {
		return nil, err
	}

	mem := readMemInfo()
	cpuCap := runtime.NumCPU()
	if cpuCap < 1 {
		cpuCap = 1
	}

	workers := resolveParserWorkers(cfg.Workers, cpuCap)
	dedup := resolveDedupWorkers(cfg.DedupWorkers, cpuCap)

	chunk := cfg.ChunkBytes
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}

	tmp := strings.TrimSpace(cfg.TempDir)
	if tmp == "" {
		absOut, err := filepath.Abs(cfg.Output)
		if err != nil {
			return nil, err
		}
		tmp = filepath.Dir(absOut)
	}

	buckets := cfg.Buckets
	var bucketsSource string
	if buckets <= 0 {
		// -od loads per-bucket dest set into RAM (8 B/key), so
		// feed auxKeyBytes to keep per-worker footprint bounded
		var auxKeyBytes int64
		if cfg.DestDedup {
			auxKeyBytes = EstimateDestKeyBytes(cfg.DestDedupDir, cfg.RunStamp)
		}
		buckets = chooseBucketCount(total, auxKeyBytes, mem, dedup, minBuckets, maxBuckets)
		bucketsSource = "auto"
	} else {
		bucketsSource = "user"
		// round up to pow2 so worker hot path can mask instead of mod
		if p := int(nextPow2(uint64(buckets))); p > buckets {
			buckets = p
			bucketsSource = "user (rounded up)"
		}
		if buckets > maxBuckets {
			buckets = maxBuckets
		}
		if buckets < 1 {
			buckets = 1
		}
	}

	// FD cap. phase 1 holds B bucket files + stdio/inputs/output (~fdReserve).
	// macOS default soft is 256, linux 1024. floor at minBuckets
	fdClamped := false
	if maxFD, ok := fdlimit.MaxOpenFiles(); ok && maxFD > 0 {
		fdCap := maxFD - fdReserve
		if fdCap < minBuckets {
			return nil, fmt.Errorf(
				"file descriptor ulimit (%d) is too low; need at least %d. Raise it with `ulimit -n %d` and retry",
				maxFD, minBuckets+fdReserve, 1024)
		}
		if buckets > fdCap {
			clamped := largestPow2AtMost(fdCap)
			if clamped < minBuckets {
				clamped = minBuckets
			}
			buckets = clamped
			bucketsSource += " (fd-clamped)"
			fdClamped = true
		}
	}

	// -od safety: if fd-clamp forced B below aux-key floor, trade
	// parallelism for memory so per-worker dest set stays ~256 MiB
	if fdClamped && cfg.DestDedup {
		const perWorkerDestSetCap = int64(256 << 20)
		auxKeyBytes := EstimateDestKeyBytes(cfg.DestDedupDir, cfg.RunStamp)
		if auxKeyBytes > 0 && buckets > 0 {
			perBucket := auxKeyBytes / int64(buckets)
			if perBucket > 0 {
				workersThatFit := int(perWorkerDestSetCap / perBucket)
				if workersThatFit < 1 {
					workersThatFit = 1
				}
				if workersThatFit < dedup {
					if cfg.Debug != nil {
						cfg.Debug.Event(
							"[od] dedup workers reduced from %d to %d: fd-clamped B=%d × library would exceed per-worker dest-set budget (~%d MiB per bucket)",
							dedup, workersThatFit, buckets, perBucket>>20)
					}
					dedup = workersThatFit
				}
			}
		}
	}

	if err := ensureNoOutputCollision(cfg.Output, cfg.Inputs); err != nil {
		return nil, err
	}

	// -od forces bucketed path, fast path has no notion of per-bucket dest sets
	useFast := !cfg.FastPathOff && !cfg.DestDedup && shouldUseFastPath(total, mem)

	return &Resolved{
		Cfg:            cfg,
		TotalInputs:    total,
		InputFileCount: len(cfg.Inputs),
		mem:            mem,
		UseFastPath:    useFast,
		chunkBytes:     chunk,
		BucketCount:    buckets,
		bucketsSource:  bucketsSource,
		Workers:        workers,
		DedupWorkers:   dedup,
		TempDir:        tmp,
	}, nil
}

// fast path = single in-RAM hashset, when inputs fit comfortably in MemAvail
func shouldUseFastPath(inputBytes int64, mem memInfo) bool {
	avail := mem.effectiveAvailable()
	if inputBytes <= 0 || avail == 0 {
		return false
	}
	if inputBytes > maxFastPathInputBytes {
		return false
	}
	if avail < uint64(fastPathMinAvailMB)*1024*1024 {
		return false
	}
	return uint64(inputBytes) < avail/uint64(fastPathRAMRatio)
}

// dispatches to fast path or bucketed pipeline
func Run(ctx context.Context, r *Resolved, m *Metrics) error {
	if r.UseFastPath {
		return runFastPath(ctx, r, m)
	}
	return runBucketed(ctx, r, m)
}

// phase 1 -> phase 2 w/ on-disk shards
func runBucketed(ctx context.Context, r *Resolved, m *Metrics) error {
	EnsureDestDedupMetrics(r)
	// publish phase + chunk totals early so TUI shows non-zero progress
	// while prepareTempDir/bucket creation run. -od enters phase 0 first
	if r.Cfg.DestDedup {
		m.Phase.Store(phasePhase0)
	} else {
		m.Phase.Store(phaseShard)
	}
	chunk := r.chunkBytes
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}
	jobs, err := buildChunkJobs(r.Cfg.Inputs, chunk, r.Cfg.NoEncodingSniff)
	if err != nil {
		return err
	}
	m.ChunksTotal.Store(int64(len(jobs)))

	// per-run subdir, removed on every return path. orphans from
	// prior crashed runs swept by main so fast path benefits too
	runDir, err := PrepareTempDir(r.TempDir, r.Cfg.RunStamp)
	if err != nil {
		return err
	}
	defer os.RemoveAll(runDir)
	// force-exit safety net: 2nd Ctrl-C bypasses deferred RemoveAll,
	// cleanup registry surfaces this path to the user
	RegisterCleanupPath(runDir)
	r.Cfg.Debug.Event("runDir: %s", runDir)

	stopDbg := startDebugProgress(ctx, r.Cfg.Debug, m, r.OdMetrics)
	defer stopDbg()

	// phase 0 (-od): discover prior archives, regen missing/stale sidecars, and
	// upgrade legacy v2 sidecars to sorted v3. dedup reads each bucket's library
	// keys directly from these sidecars (no per-run routing into scratch).
	//
	// Runs CONCURRENTLY with shard so the cold-run regen/upgrade cost overlaps
	// input parsing; we block on it only before dedup, the first step that needs
	// the sidecars. destSidecars / r.odResult / odErr are written by the
	// goroutine and read after <-odDone (channel close = happens-before).
	var destSidecars []string
	var odErr error
	odDone := make(chan struct{})
	odRunning := false
	odCtx, odCancel := context.WithCancel(ctx)
	defer odCancel()
	if r.Cfg.DestDedup {
		// fail fast on a bad dest dir BEFORE shard does work — the scan now runs
		// concurrently, so a missing/non-dir would otherwise surface only after
		// shard wasted effort.
		if fi, statErr := os.Stat(r.Cfg.DestDedupDir); statErr != nil {
			return fmt.Errorf("od-scan: dest dir: %w", statErr)
		} else if !fi.IsDir() {
			return fmt.Errorf("od-scan: dest dir: %s is not a directory", r.Cfg.DestDedupDir)
		}
		odRunning = true
		m.Phase.Store(phaseShard) // OD frame stacks below the shard frame
		go func() {
			defer close(odDone)
			odRes, err := runODScan(odCtx, odConfig{
				Dest:            r.Cfg.DestDedupDir,
				CurrentRunStamp: r.Cfg.RunStamp,
				Buckets:         r.BucketCount,
				TempDir:         runDir,
				Debug:           r.Cfg.Debug,
			}, r.OdMetrics)
			if err != nil {
				odErr = err
				return
			}
			r.OdResult = odRes
			destSidecars = odRes.DestSidecarPaths
		}()
	}

	tShard := time.Now()
	if r.Cfg.Debug != nil {
		r.Cfg.Debug.Printf("PHASE shard START\n")
		r.Cfg.Debug.Flush()
	}
	res, err := shard(ctx, shardConfig{
		inputs:     r.Cfg.Inputs,
		jobs:       jobs,
		tempDir:    runDir,
		buckets:    r.BucketCount,
		workers:    r.Workers,
		chunkBytes: r.chunkBytes,
		noURI:      r.Cfg.NoURI,
		loose:      r.Cfg.Loose,
		reject:     r.Cfg.Reject,
	}, m)
	if err != nil {
		if odRunning { // stop the -od scan and drain before returning
			odCancel()
			<-odDone
		}
		return fmt.Errorf("shard: %w", err)
	}
	if r.Cfg.Debug != nil {
		r.Cfg.Debug.printfPhase("PHASE shard END", time.Since(tShard))
	}

	// block on phase 0 before dedup (first step that needs the library sidecars).
	// parsing done but library prep still running → switch to phasePhase0 so
	// the TUI shows LIBRARY PREP instead of a frozen [1/3 PARSING] bar.
	if odRunning {
		if odPhaseInFlight(r.OdMetrics) {
			m.Phase.Store(phasePhase0)
		}
		<-odDone
		if odErr != nil {
			return fmt.Errorf("od-scan: %w", odErr)
		}
	}

	m.Phase.Store(phaseDedup)
	tDedup := time.Now()
	if r.Cfg.Debug != nil {
		r.Cfg.Debug.Printf("PHASE dedup START\n")
		r.Cfg.Debug.Flush()
	}

	sink, err := newLineSink(r)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = sink.close()
		if !success {
			removeOutputFiles(sinkOutputPaths(sink))
		}
	}()
	for _, p := range sinkOutputPaths(sink) {
		RegisterCleanupPath(p)
	}

	nBucket := len(res.bucketPaths)
	if nBucket == 0 {
		if r.Cfg.Debug != nil {
			r.Cfg.Debug.printfPhase("PHASE dedup END (no buckets to process)", time.Since(tDedup))
		}
		success = true
		r.OutputPaths = sinkOutputPaths(sink)
		m.Phase.Store(phaseDone)
		return nil
	}

	// sum bucket file sizes so dedup bar advances at byte granularity.
	// stat fail = bytesTotal stays 0, renderer falls back to bucketsDone ratio
	var bucketBytes int64
	for _, p := range res.bucketPaths {
		if fi, err := os.Stat(p); err == nil {
			bucketBytes += fi.Size()
		}
	}
	m.BucketsBytesTotal.Store(bucketBytes)

	if _, err := dedup(ctx, dedupConfig{
		bucketPaths:  res.bucketPaths,
		destSidecars: destSidecars,
		odMetrics:    r.OdMetrics,
		workers:      r.DedupWorkers,
	}, sink, m); err != nil {
		return fmt.Errorf("dedup: %w", err)
	}
	if r.Cfg.Debug != nil {
		r.Cfg.Debug.printfPhase("PHASE dedup END", time.Since(tDedup))
	}

	// flush sink so archive is finalized + renamed BEFORE sidecars,
	// sinkOutputPaths only authoritative post-close
	if err := sink.close(); err != nil {
		return fmt.Errorf("sink close: %w", err)
	}
	r.OutputPaths = sinkOutputPaths(sink)

	if r.Cfg.DestDedup && r.Cfg.Debug != nil {
		for _, p := range r.OutputPaths {
			hdr, err := readSidecarHeader(sidecarPathForArchive(p))
			if err != nil {
				r.Cfg.Debug.Event("[od] inline sidecar: path=%s read err=%v", filepath.Base(p), err)
				continue
			}
			r.Cfg.Debug.Event("[od] inline sidecar: path=%s keys=%d", filepath.Base(p), hdr.keyCount)
		}
	}

	success = true
	m.Phase.Store(phaseDone)
	return nil
}

// single-goroutine read+parse+dedup, in-RAM hashset, skips shards
func runFastPath(ctx context.Context, r *Resolved, m *Metrics) error {
	stopDbg := startDebugProgress(ctx, r.Cfg.Debug, m, nil)
	defer stopDbg()

	m.Phase.Store(phaseShard) // single phase, reuse SHARDING label
	if m != nil {
		m.ChunksTotal.Store(int64(len(r.Cfg.Inputs)))
		m.BucketsTotal.Store(1)
	}

	tFast := time.Now()
	if r.Cfg.Debug != nil {
		r.Cfg.Debug.Printf("PHASE fastpath START (single-phase read+dedup)\n")
		r.Cfg.Debug.Flush()
	}

	sink, err := newLineSink(r)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = sink.close()
		if !success {
			removeOutputFiles(sinkOutputPaths(sink))
		}
	}()
	for _, p := range sinkOutputPaths(sink) {
		RegisterCleanupPath(p)
	}

	seen := make(map[uint64]struct{}, 1<<16)
	// shared per-run state, single goroutine = no sync needed
	br := bufio.NewReaderSize(nil, defaultReadBufBytes)
	lf := newLineFormatter()

	for _, p := range r.Cfg.Inputs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := fastPathFile(p, seen, sink, br, lf, r.Cfg.NoURI, r.Cfg.Loose, r.Cfg.NoEncodingSniff, m, r.Cfg.Reject); err != nil {
			return err
		}
		if m != nil {
			m.ChunksDone.Add(1)
		}
	}

	if m != nil {
		m.BucketsDone.Store(1)
	}
	if err := sink.close(); err != nil {
		return err
	}
	success = true
	r.OutputPaths = sinkOutputPaths(sink)
	if r.Cfg.Debug != nil {
		r.Cfg.Debug.printfPhase("PHASE fastpath END", time.Since(tFast))
	}
	m.Phase.Store(phaseDone)
	return nil
}

// streams one file, writes first-seen records. shared map/sink/reader/lf
// across files so hot loop is alloc-free after warmup. BOM sniff routes
// UTF-16 via transform.Reader so loop sees UTF-8
func fastPathFile(path string, seen map[uint64]struct{}, sink lineSink, br *bufio.Reader, lf *lineFormatter, noURI, loose, noEncodingSniff bool, m *Metrics, rr *RejectRecorder) error {
	absPath, aerr := filepath.Abs(path)
	if aerr != nil {
		absPath = path
	}

	enc, bomBytes := encUTF8, 0
	if !noEncodingSniff {
		var err error
		enc, bomBytes, err = sniffEncoding(path)
		if err != nil {
			return err
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if bomBytes > 0 {
		if _, err := io.CopyN(io.Discard, f, int64(bomBytes)); err != nil {
			return err
		}
	}

	var src io.Reader = f
	var counter *countingReader
	if enc == encUTF16LE || enc == encUTF16BE {
		counter = &countingReader{r: f}
		src = wrapReader(counter, enc)
	}
	br.Reset(src)
	var lineNum int64
	for {
		var rawBefore int64
		if counter != nil {
			rawBefore = counter.n.Load()
		}
		line, consumed, tooLong, rerr := readBoundedLine(br, maxInputLineBytes)
		if consumed > 0 {
			if m != nil {
				if counter != nil {
					m.BytesRead.Add(counter.n.Load() - rawBefore)
				} else {
					m.BytesRead.Add(consumed)
				}
			}
			if tooLong {
				lineNum++
				if m != nil {
					m.LinesRead.Add(1)
					m.LinesRejected.Add(1)
				}
				if rr != nil {
					rr.Record(absPath, strconv.FormatInt(lineNum, 10), "<line too long>")
				}
			} else {
				trimmed := strings.TrimRight(line, "\r\n")
				if trimmed != "" {
					lineNum++
					if m != nil {
						m.LinesRead.Add(1)
					}
					host, url, login, password, ok := parseFor(trimmed, loose)
					if !ok {
						if m != nil {
							m.LinesRejected.Add(1)
						}
						if rr != nil {
							rr.Record(absPath, strconv.FormatInt(lineNum, 10), trimmed)
						}
					} else {
						if m != nil {
							m.LinesAccepted.Add(1)
						}
						h := lf.HashKey(host, login, password)
						if _, dup := seen[h]; !dup {
							seen[h] = struct{}{}
							out, repr := lf.FormatRecordStableLine(host, url, login, password, noURI)
							if !repr {
								// no round-trippable form: drop. undo the
								// parse-time accepted tick and count as rejected.
								if m != nil {
									m.LinesAccepted.Add(-1)
									m.LinesRejected.Add(1)
									m.LinesUnrepresentable.Add(1)
								}
								if rr != nil {
									rr.Record(absPath, strconv.FormatInt(lineNum, 10), trimmed)
								}
							} else if err := sink.writeBatch(out, 1, m); err != nil {
								return err
							}
						}
					}
				}
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

// sum of file sizes, err if any stat fails
func totalInputBytes(inputs []string) (int64, error) {
	var total int64
	for _, p := range inputs {
		info, err := os.Stat(p)
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

// resolves path to sorted list of .txt files. file = must end .txt,
// dir = recursive walk. stdin "-" rejected explicitly (no streaming path)
func CollectInputs(path string) ([]string, error) {
	if path == "-" {
		return nil, fmt.Errorf("stdin not supported; pass a file or directory")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if !strings.EqualFold(filepath.Ext(path), ".txt") {
			return nil, fmt.Errorf("input file must end in .txt: %s", path)
		}
		return []string{path}, nil
	}
	files := make([]string, 0, 64)
	err = filepath.WalkDir(path, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(p), ".txt") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .txt files found under: %s", path)
	}
	sort.Strings(files)
	return files, nil
}

// resolveParserWorkers picks the phase-1 parser goroutine count. An explicit
// positive flag wins; otherwise it scales with the box's cores (no hard cap,
// so stronger machines aren't throttled to 8). cpu is the detected core count.
func resolveParserWorkers(flag, cpu int) int {
	if flag > 0 {
		return flag
	}
	return clampInt(cpu, 1, cpu)
}

// resolveDedupWorkers picks the phase-2 dedup goroutine count. Each dedup
// worker holds a bucket's dest set in RAM, so it keeps the half-cores ratio,
// but the ceiling grows with cores instead of a fixed 4.
func resolveDedupWorkers(flag, cpu int) int {
	if flag > 0 {
		return flag
	}
	return clampInt(cpu/2, 1, cpu)
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// rejects output==input. dedup truncates the file mid-run otherwise,
// user loses data on re-run. pathident.SameFile catches symlinks/case folds
func ensureNoOutputCollision(output string, inputs []string) error {
	absOut, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	absOut = filepath.Clean(absOut)
	for _, in := range inputs {
		absIn, err := filepath.Abs(in)
		if err != nil {
			return err
		}
		absIn = filepath.Clean(absIn)
		if absIn == absOut {
			return fmt.Errorf("output path collides with an input file: %s", absOut)
		}
		same, sErr := pathident.SameFile(absOut, absIn)
		if sErr == nil && same {
			return fmt.Errorf("output path collides with an input file: %s", absOut)
		}
	}
	return nil
}
