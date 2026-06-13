package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
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

	cfg := pipelineConfig{
		Inputs:       []string{in},
		Output:       filepath.Join(d, "out.txt"),
		TempDir:      filepath.Join(d, "shards"),
		Workers:      2,
		DedupWorkers: 2,
		Buckets:      8,
		ChunkBytes:   1 << 20,
		FastPathOff:  true, // exercise bucketed path
	}
	r, err := resolvePipelineConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &metrics{totalInputBytes: r.totalInputs}
	if err := run(context.Background(), r, m); err != nil {
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
	if m.linesUnique.Load() != int64(len(want)) {
		t.Fatalf("metrics.linesUnique = %d, want %d", m.linesUnique.Load(), len(want))
	}
	if m.linesRejected.Load() != 1 {
		t.Fatalf("metrics.linesRejected = %d, want 1", m.linesRejected.Load())
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

	cfg := pipelineConfig{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		TempDir: filepath.Join(d, "shards"),
		Workers: 1,
	}
	r, err := resolvePipelineConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// force fast path even w/o meminfo (CI)
	r.useFastPath = true

	m := &metrics{totalInputBytes: r.totalInputs}
	if err := run(context.Background(), r, m); err != nil {
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
	stamp := runStamp(started, "ftztst")
	firstZst, err := filepath.Abs(filepath.Join(d, withZstExt(defaultBasename(stamp), true)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := pipelineConfig{
		Inputs:        []string{in},
		Output:        firstZst,
		TempDir:       filepath.Join(d, "shards"),
		Workers:       1,
		Compress:      true,
		ZstChunkLines: 2,
		RunStarted:    started,
		RunStamp:      stamp,
	}
	r, err := resolvePipelineConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.useFastPath = true
	m := &metrics{totalInputBytes: r.totalInputs}
	if err := run(context.Background(), r, m); err != nil {
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

	got, err := collectInputs(a)
	if err != nil || len(got) != 1 || got[0] != a {
		t.Fatalf("single file: %v %v", got, err)
	}

	got, err = collectInputs(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("dir scan must skip non-txt; got %v", got)
	}
}

func TestCollectInputsRejectsStdin(t *testing.T) {
	_, err := collectInputs("-")
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

	r, err := resolvePipelineConfig(pipelineConfig{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		Buckets: 100, // rounds to 128
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.bucketCount != 128 {
		t.Fatalf("bucketCount = %d, want 128", r.bucketCount)
	}

	r, err = resolvePipelineConfig(pipelineConfig{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		Buckets: 999_999,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.bucketCount != maxBuckets {
		t.Fatalf("bucketCount = %d, want clamp to %d", r.bucketCount, maxBuckets)
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
	r := &resolved{
		cfg: pipelineConfig{
			Inputs: []string{in},
			Output: filepath.Join(d, "out.txt"),
		},
		useFastPath: true,
	}
	m := &metrics{}
	if err := run(context.Background(), r, m); err == nil {
		t.Fatal("expected error from missing input")
	}
	if _, err := os.Stat(r.cfg.Output); !os.IsNotExist(err) {
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

	r, err := resolvePipelineConfig(pipelineConfig{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     tempParent,
		FastPathOff: true,
		Buckets:     4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), &resolved{
		cfg:          r.cfg,
		totalInputs:  r.totalInputs,
		mem:          r.mem,
		bucketCount:  4,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      tempParent,
	}, &metrics{}); err != nil {
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
