package ulpengine

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestEndToEndBucketed(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com/p1:user@example.com:p1\n"+
			"https://a.example.com/p2:user@example.com:p1\n"+ // same dedup key
			"not-a-line\n"+
			"https://b.example.com:user2:p2\n"+
			"https://b.example.com:user2:p2\n"+ // exact dup
			"https://c.example.com:user3:p3\n",
	)

	cfg := Config{
		Inputs:       []string{in},
		Output:       filepath.Join(d, "out.txt"),
		TempDir:      filepath.Join(d, "shards"),
		Workers:      2,
		DedupWorkers: 2,
		Buckets:      8,
		ChunkBytes:   1 << 20,
		FastPathOff:  true, // exercise bucketed path
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, cfg.Output)
	sort.Strings(got)
	want := []string{
		"a.example.com/p1:user@example.com:p1",
		"b.example.com:user2:p2",
		"c.example.com:user3:p3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("output mismatch\n got: %v\nwant: %v", got, want)
	}
	if m.LinesUnique.Load() != int64(len(want)) {
		t.Fatalf("metrics.linesUnique = %d, want %d", m.LinesUnique.Load(), len(want))
	}
	if m.LinesRejected.Load() != 1 {
		t.Fatalf("metrics.linesRejected = %d, want 1", m.LinesRejected.Load())
	}

	tmpEntries, err := os.ReadDir(cfg.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tmpEntries {
		if strings.HasPrefix(e.Name(), "shard_") {
			t.Fatalf("shard temp not cleaned up: %s", e.Name())
		}
	}
}

func TestEndToEndFastPath(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com:user:p\n"+
			"https://a.example.com:user:p\n"+
			"https://b.example.com:user:p\n",
	)

	cfg := Config{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		TempDir: filepath.Join(d, "shards"),
		Workers: 1,
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// force fast path even w/o meminfo (CI)
	r.UseFastPath = true

	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}

	got := readLines(t, cfg.Output)
	sort.Strings(got)
	want := []string{
		"a.example.com:user:p",
		"b.example.com:user:p",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("fast path output mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestEndToEndFastPathZstdChunked(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com:user:p1\n"+
			"https://b.example.com:user:p2\n"+
			"https://c.example.com:user:p3\n",
	)
	started := time.Date(2026, 5, 10, 15, 4, 5, 0, time.UTC)
	stamp := RunStamp(started, "ftztst")
	firstZst, err := filepath.Abs(filepath.Join(d, WithZstExt(DefaultBasename(stamp), true)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Inputs:        []string{in},
		Output:        firstZst,
		TempDir:       filepath.Join(d, "shards"),
		Workers:       1,
		Compress:      true,
		ZstChunkLines: 2,
		RunStarted:    started,
		RunStamp:      stamp,
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.UseFastPath = true
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	if len(r.OutputPaths) < 2 {
		t.Fatalf("expected multiple zst parts, got %v", r.OutputPaths)
	}
	var lines []string
	for _, p := range r.OutputPaths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := zstd.NewReader(f)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, copyErr := io.Copy(&buf, dec)
		dec.Close()
		_ = f.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		body := strings.TrimRight(buf.String(), "\n")
		if body != "" {
			lines = append(lines, strings.Split(body, "\n")...)
		}
	}
	sort.Strings(lines)
	want := []string{
		"a.example.com:user:p1",
		"b.example.com:user:p2",
		"c.example.com:user:p3",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("decompressed mismatch\n got: %v\nwant: %v", lines, want)
	}
}

func TestCollectInputsSingleAndDir(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a.txt")
	b := filepath.Join(d, "sub", "b.txt")
	c := filepath.Join(d, "sub", "c.csv")
	writeFile(t, a, "x")
	if err := os.MkdirAll(filepath.Dir(b), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, b, "y")
	writeFile(t, c, "z")

	got, err := CollectInputs(a)
	if err != nil || len(got) != 1 || got[0] != a {
		t.Fatalf("single file: %v %v", got, err)
	}

	got, err = CollectInputs(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("dir scan must skip non-txt; got %v", got)
	}
}

func TestCollectInputsRejectsStdin(t *testing.T) {
	_, err := CollectInputs("-")
	if err == nil {
		t.Fatal("expected error for stdin sentinel")
	}
	if !strings.Contains(err.Error(), "stdin not supported") {
		t.Fatalf("error = %v; want substring 'stdin not supported'", err)
	}
}

func TestResolveRoundsUserBucketsToPow2(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "https://a.example.com:user:p\n")

	r, err := Resolve(Config{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		Buckets: 100, // rounds to 128
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.BucketCount != 128 {
		t.Fatalf("bucketCount = %d, want 128", r.BucketCount)
	}

	r, err = Resolve(Config{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		Buckets: 999_999,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.BucketCount != maxBuckets {
		t.Fatalf("bucketCount = %d, want clamp to %d", r.BucketCount, maxBuckets)
	}
}

func TestEnsureNoOutputCollisionRejects(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "x")

	if err := ensureNoOutputCollision(in, []string{in}); err == nil {
		t.Fatal("expected error when output equals input")
	}
	rel, err := filepath.Rel(d, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureNoOutputCollision(filepath.Join(d, rel), []string{in}); err == nil {
		t.Fatal("expected error after Abs+Clean even when output uses relative form")
	}
	if err := ensureNoOutputCollision(filepath.Join(d, "other.txt"), []string{in}); err != nil {
		t.Fatalf("unexpected error for distinct output: %v", err)
	}
}

func TestRunBucketedRemovesPartialOutputOnFastPathError(t *testing.T) {
	d := t.TempDir()
	// fast path + missing input, partial output must be removed
	in := filepath.Join(d, "missing.txt")
	r := &Resolved{
		Cfg: Config{
			Inputs: []string{in},
			Output: filepath.Join(d, "out.txt"),
		},
		UseFastPath: true,
	}
	m := &Metrics{}
	if err := Run(context.Background(), r, m); err == nil {
		t.Fatal("expected error from missing input")
	}
	if _, err := os.Stat(r.Cfg.Output); !os.IsNotExist(err) {
		t.Fatalf("partial output should be removed, stat err = %v", err)
	}
}

func TestRunBucketedRemovesShardSubdirAfterSuccess(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com:user:p\nhttps://b.example.com:user:p\n",
	)
	tempParent := filepath.Join(d, "stage")

	r, err := Resolve(Config{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     tempParent,
		FastPathOff: true,
		Buckets:     4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      tempParent,
	}, &Metrics{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tempParent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempSubdirPrefix) {
			t.Fatalf("shard subdir %q not cleaned up", e.Name())
		}
	}
}

func TestShouldUseFastPathHonorsAbsoluteInputCap(t *testing.T) {
	mem := memInfo{available: 64 << 30}
	if shouldUseFastPath((1<<30)+1, mem) {
		t.Fatal("fast path should be disabled above the absolute input cap")
	}
}

func TestShouldUseFastPathAllowsSmallInputUnderMemoryThreshold(t *testing.T) {
	mem := memInfo{available: 64 << 30}
	if !shouldUseFastPath(512<<20, mem) {
		t.Fatal("fast path should be enabled below the absolute cap and memory threshold")
	}
}

func TestShouldUseFastPathAllowsInputAtAbsoluteCap(t *testing.T) {
	mem := memInfo{available: 64 << 30}
	if !shouldUseFastPath(1<<30, mem) {
		t.Fatal("fast path should remain enabled at the absolute input cap")
	}
}

func TestLargestPow2AtMost(t *testing.T) {
	cases := map[int]int{
		0:    0,
		1:    1,
		2:    2,
		3:    2,
		255:  128,
		256:  256,
		1023: 512,
		1024: 1024,
	}
	for n, want := range cases {
		if got := largestPow2AtMost(n); got != want {
			t.Errorf("largestPow2AtMost(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestBucketBufBytesScales(t *testing.T) {
	cases := map[int]int{
		1:     bucketWriterBufCeilBytes,         // tiny B clamps to ceil
		64:    bucketWriterBufCeilBytes,         // 4MiB > ceil
		256:   bucketWriterBufCeilBytes,         // 1MiB == ceil
		4096:  bucketWriterBufTotalBytes / 4096, // 64 KiB
		16384: bucketWriterBufFloorBytes,        // floor
	}
	for B, want := range cases {
		if got := bucketBufBytes(B); got != want {
			t.Errorf("bucketBufBytes(%d) = %d, want %d", B, got, want)
		}
	}
}

func TestChooseBucketCountUsesEffectiveAvailable(t *testing.T) {
	// cgroup quota < host MemAvailable must steer toward cgroup
	hostOnly := chooseBucketCount(50<<30, 0, memInfo{available: 32 << 30}, 4, minBuckets, maxBuckets)
	cgroupSqueezed := chooseBucketCount(50<<30, 0, memInfo{available: 32 << 30, cgroupLimit: 2 << 30}, 4, minBuckets, maxBuckets)
	if cgroupSqueezed <= hostOnly {
		t.Fatalf("cgroup choice (%d) should be > host-only (%d), smaller per-bucket = more buckets",
			cgroupSqueezed, hostOnly)
	}
}

func TestChooseBucketCountSensible(t *testing.T) {
	// small input + plenty of RAM = min buckets
	b := chooseBucketCount(1<<20, 0, memInfo{total: 64 << 30, available: 32 << 30}, 4, minBuckets, maxBuckets)
	if b != minBuckets {
		t.Errorf("small input: B=%d, want %d", b, minBuckets)
	}
	// huge input on small box = maxBuckets
	b = chooseBucketCount(1<<50, 0, memInfo{total: 8 << 30, available: 1 << 30}, 4, minBuckets, maxBuckets)
	if b != maxBuckets {
		t.Errorf("huge input: B=%d, want %d", b, maxBuckets)
	}
	// all pow2
	b = chooseBucketCount(50<<30, 0, memInfo{total: 32 << 30, available: 16 << 30}, 4, minBuckets, maxBuckets)
	if b&(b-1) != 0 {
		t.Errorf("B should be a power of two, got %d", b)
	}
}

// large -od auxKeyBytes must force B high enough to keep per-bucket
// dest-set <= 128 MiB. pre-fix: huge library + roomy box = B=64 = GBs/worker
func TestChooseBucketCountODAuxKeyFloor(t *testing.T) {
	// 10e9 keys * 8 bytes = 80 GB
	const libBytes = 10_000_000_000 * 8
	b := chooseBucketCount(int64(100<<30), int64(libBytes),
		memInfo{total: 64 << 30, available: 48 << 30}, 4, minBuckets, maxBuckets)
	// 80 GB / 128 MiB = 640 -> 1024
	if b < 1024 {
		t.Errorf("-od aux floor: B=%d, want >= 1024", b)
	}
	bNoOD := chooseBucketCount(int64(100<<30), 0,
		memInfo{total: 64 << 30, available: 48 << 30}, 4, minBuckets, maxBuckets)
	if bNoOD >= b {
		t.Errorf("no-od B (%d) should be < -od B (%d), aux floor not engaging", bNoOD, b)
	}
}

// runODIngest drives one bucketed -od ingest against libDir with the given
// (possibly sfu_-prefixed) stamp and returns the run's metrics.
func runODIngest(t *testing.T, libDir, stamp, input string) *Metrics {
	t.Helper()
	in := filepath.Join(t.TempDir(), "in.txt")
	writeFile(t, in, input)
	stage := filepath.Join(libDir, ".stage")
	r, err := Resolve(Config{
		Inputs:       []string{in},
		Output:       filepath.Join(libDir, WithZstExt(DefaultBasename(stamp), true)),
		TempDir:      stage,
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     stamp,
	})
	if err != nil {
		t.Fatalf("resolve %s: %v", stamp, err)
	}
	m := &Metrics{}
	if err := Run(context.Background(), &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      stage,
	}, m); err != nil {
		t.Fatalf("run %s: %v", stamp, err)
	}
	return m
}

// TestODRetryExcludesCurrentRunArchive: a retry with an existing
// sfu_<stamp>.txt.zst in the library must exclude exactly the output run from
// destination discovery — for BOTH caller stamp forms (bare and sfu_-prefixed,
// which parseArchiveName reports identically) — while older runs remain dedup
// sources. Before normalization, the current run's own archive was a dedup
// source AND was overwritten at the same path, dropping records the retry
// input still contained.
func TestODRetryExcludesCurrentRunArchive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp string
	}{
		{"unprefixed stamp", "retry_st1"},
		{"prefixed stamp", "sfu_retry_st2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			libDir := t.TempDir()

			// older run: must remain a dedup source through the retry
			runODIngest(t, libDir, "older_st", "https://old.example.com:ou:op\n")
			oldArchive := filepath.Join(libDir, "sfu_older_st.txt.zst")
			oldBytes, err := os.ReadFile(oldArchive)
			if err != nil {
				t.Fatalf("read older archive: %v", err)
			}

			// current run, first pass: creates its own archive + sidecar
			runODIngest(t, libDir, tc.stamp, "https://cur.example.com:cu:cp\n")

			// retry with the same stamp: input dups the older run AND the
			// current run's own previous archive, plus one new line.
			m := runODIngest(t, libDir, tc.stamp, strings.Join([]string{
				"https://old.example.com:ou:op", // dup of older run: skipped
				"https://cur.example.com:cu:cp", // dup of own archive: must be KEPT
				"https://new.example.com:nu:np", // new
			}, "\n")+"\n")

			if got := m.LinesSkippedByDest.Load(); got != 1 {
				t.Errorf("linesSkippedByDest = %d, want 1 (only the older run's dup)", got)
			}
			out := readZstdLines(t, filepath.Join(libDir, WithZstExt(DefaultBasename(tc.stamp), true)))
			if len(out) != 2 {
				t.Fatalf("retry output has %d lines, want 2: %v", len(out), out)
			}
			for _, want := range []string{
				"cur.example.com:cu:cp", // own-archive dup survives
				"new.example.com:nu:np",
			} {
				if !slices.Contains(out, want) {
					t.Errorf("retry output missing %q; got %v", want, out)
				}
			}
			if slices.Contains(out, "old.example.com:ou:op") {
				t.Error("retry output must not contain the older run's dup")
			}

			// the older run's archive was read, never modified
			newBytes, err := os.ReadFile(oldArchive)
			if err != nil {
				t.Fatalf("re-read older archive: %v", err)
			}
			if !bytes.Equal(oldBytes, newBytes) {
				t.Error("older run's archive was modified by the retry")
			}
		})
	}
}

// TestEstimateDestKeyBytesExcludesCurrentRun: the bucket sizer must exclude
// the current run's own sidecar, using the run ID in parseArchiveName form.
func TestEstimateDestKeyBytesExcludesCurrentRun(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "sfu_self_st.txt.zst")
	other := filepath.Join(dir, "sfu_other_st.txt.zst")
	if err := os.WriteFile(self, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	selfKeys := writeSidecarKeysForTest(t, self, []uint64{1, 2, 3})
	otherKeys := writeSidecarKeysForTest(t, other, []uint64{4, 5})

	if got := EstimateDestKeyBytes(dir, "sfu_self_st"); got != int64(otherKeys)*SidecarKeyBytes {
		t.Errorf("EstimateDestKeyBytes(exclude sfu_self_st) = %d, want %d", got, otherKeys*SidecarKeyBytes)
	}
	// raw caller stamps normalize to the same run ID
	if got := EstimateDestKeyBytes(dir, "self_st"); got != int64(otherKeys)*SidecarKeyBytes {
		t.Errorf("EstimateDestKeyBytes(exclude self_st) = %d, want %d", got, otherKeys*SidecarKeyBytes)
	}
	// no exclusion counts both
	if got := EstimateDestKeyBytes(dir, ""); got != int64(selfKeys+otherKeys)*SidecarKeyBytes {
		t.Errorf("EstimateDestKeyBytes(no exclude) = %d, want %d", got, (selfKeys+otherKeys)*SidecarKeyBytes)
	}
}
