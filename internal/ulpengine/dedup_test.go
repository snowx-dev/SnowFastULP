package ulpengine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// hand-built bucket file matching shard.go layout
// [u64 hash][u32 line_len][line bytes], lets us test dedup w/o phase 1
func writeBucket(t *testing.T, p string, recs []bucketRecord) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	var hdr [bucketRecordHeaderBytes]byte
	for _, r := range recs {
		binary.LittleEndian.PutUint64(hdr[0:8], r.hash)
		binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(r.line)))
		if _, err := bw.Write(hdr[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := bw.WriteString(r.line); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return nil
	}
	out := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return out
}

func TestDedupBucketDropsDuplicateHashes(t *testing.T) {
	d := t.TempDir()
	bucketPath := filepath.Join(d, "shard_00000.bin")
	writeBucket(t, bucketPath, []bucketRecord{
		{hash: 1, line: "a.example.com:user:p"},
		{hash: 1, line: "a.example.com/user:p"}, // same hash, distinct tuple: kept
		{hash: 2, line: "b.example.com:user:q"},
		{hash: 1, line: "a.example.com:user:p"}, // exact duplicate: dropped
	})

	out := filepath.Join(d, "out.txt")
	m := &Metrics{}
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := dedup(context.Background(), dedupConfig{
		bucketPaths: []string{bucketPath},
		workers:     1,
	}, sink, m)
	if err != nil {
		_ = sink.abort()
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("want 3 unique lines (one exact dup dropped), got %d", n)
	}
	got := readLines(t, out)
	if len(got) != 3 {
		t.Fatalf("output has %d lines: %v", len(got), got)
	}
	if got[0] != "a.example.com:user:p" || got[1] != "a.example.com/user:p" || got[2] != "b.example.com:user:q" {
		t.Fatalf("unexpected first-seen order: %v", got)
	}
	// bucket file removed after clean drain
	if _, err := os.Stat(bucketPath); !os.IsNotExist(err) {
		t.Fatalf("bucket file should have been removed, stat err = %v", err)
	}
}

func TestDedupParallelOverManyBuckets(t *testing.T) {
	d := t.TempDir()
	const B = 16
	const perBucket = 50
	bucketPaths := make([]string, B)
	for b := 0; b < B; b++ {
		p := filepath.Join(d, defaultBucketName(b))
		recs := make([]bucketRecord, 0, perBucket*2)
		for i := 0; i < perBucket; i++ {
			h := uint64(b)<<32 | uint64(i)
			line := "host" + strconv.Itoa(b) + ".example.com:user:" + strconv.Itoa(i)
			recs = append(recs, bucketRecord{hash: h, line: line})
			recs = append(recs, bucketRecord{hash: h, line: line}) // exact duplicate
		}
		writeBucket(t, p, recs)
		bucketPaths[b] = p
	}

	out := filepath.Join(d, "out.txt")
	m := &Metrics{}
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := dedup(context.Background(), dedupConfig{
		bucketPaths: bucketPaths,
		workers:     4,
	}, sink, m)
	if err != nil {
		_ = sink.abort()
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	if n != int64(B*perBucket) {
		t.Fatalf("want %d unique lines, got %d", B*perBucket, n)
	}
	got := readLines(t, out)
	if len(got) != B*perBucket {
		t.Fatalf("output line count = %d", len(got))
	}
	sort.Strings(got)
	if dup := firstDuplicate(got); dup != "" {
		t.Fatalf("output contains duplicate line: %q", dup)
	}
}

func TestDedupReturnsErrorOnCanceledContext(t *testing.T) {
	d := t.TempDir()
	bucketPath := filepath.Join(d, "shard_00000.bin")
	writeBucket(t, bucketPath, []bucketRecord{{hash: 1, line: "a:b:c"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := filepath.Join(d, "out.txt")
	sink, cerr := newOutputSink(out, false, false)
	if cerr != nil {
		t.Fatal(cerr)
	}
	defer func() { _ = sink.abort() }()
	_, err := dedup(ctx, dedupConfig{
		bucketPaths: []string{bucketPath},
		workers:     2,
		keepBuckets: true,
	}, sink, &Metrics{})
	if err == nil {
		t.Fatal("dedup with pre-canceled context should return non-nil error")
	}
}

func TestWriteBatchCountsLinesAndBytes(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "out.txt")
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{}
	if err := sink.writeBatch([]byte("a\nbb\nccc\n"), 3, m); err != nil {
		t.Fatal(err)
	}
	if err := sink.writeBatch(nil, 0, m); err != nil {
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	if got := m.LinesUnique.Load(); got != 3 {
		t.Errorf("linesUnique = %d, want 3", got)
	}
	if got := m.BytesWritten.Load(); got != 9 {
		t.Errorf("bytesWritten = %d, want 9", got)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a\nbb\nccc\n" {
		t.Errorf("file contents = %q", got)
	}
}

func TestDedupBucketRejectsOversizedRecord(t *testing.T) {
	d := t.TempDir()
	bucketPath := filepath.Join(d, "bad.bin")
	// header claims record > maxRecordLineLen
	f, err := os.Create(bucketPath)
	if err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, bucketRecordHeaderBytes)
	binary.LittleEndian.PutUint64(hdr[0:8], 1)
	binary.LittleEndian.PutUint32(hdr[8:12], maxRecordLineLen+1)
	if _, err := f.Write(hdr); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(d, "out.txt")
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.abort() }()
	_, err = dedup(context.Background(), dedupConfig{
		bucketPaths: []string{bucketPath},
		workers:     1,
		keepBuckets: true,
	}, sink, &Metrics{})
	if err == nil {
		t.Fatal("expected error on oversized record header")
	}
}

// guards against encoder-close ordering bugs, flushing bufio after
// closing the encoder would corrupt the stream
func TestOutputSinkZstdRoundTrip(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "out.txt.zst")
	sink, err := newOutputSink(out, true, false)
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{}
	want := []string{
		"a.example.com:user:p1",
		"b.example.com:user:p2",
		"c.example.com:user:p3",
	}
	if err := writeLine(sink, want[0], m); err != nil {
		t.Fatal(err)
	}
	if err := sink.writeBatch([]byte(want[1]+"\n"+want[2]+"\n"), 2, m); err != nil {
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	// idempotent commit, deferred safety-net calls it a 2nd time
	if err := sink.commit(); err != nil {
		t.Fatalf("second commit should be a no-op, got %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, dec); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("decompressed line count = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}

	// compressed size < uncompressed bytes fed in, also confirms metrics
	// counter still tracks uncompressed
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, in := fi.Size(), m.BytesWritten.Load(); got >= in {
		t.Errorf("compressed size %d should be < uncompressed %d", got, in)
	}
}

// no-input early return must still emit a valid zstd stream, decoders
// reject zero-byte files
func TestDedupCompressedEmptyBucketsProducesValidZst(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "empty.txt.zst")
	sink, cerr := newOutputSink(out, true, false)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if _, err := dedup(context.Background(), dedupConfig{
		bucketPaths: nil,
		workers:     1,
	}, sink, &Metrics{}); err != nil {
		_ = sink.abort()
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatalf("expected valid zstd stream, got %v", err)
	}
	defer dec.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, dec); err != nil {
		t.Fatalf("decompressing empty .zst: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("empty input should produce empty plaintext, got %d bytes", buf.Len())
	}
}

func TestDedupKeepBuckets(t *testing.T) {
	d := t.TempDir()
	bucketPath := filepath.Join(d, "shard_00000.bin")
	writeBucket(t, bucketPath, []bucketRecord{{hash: 1, line: "x:y:z"}})
	out := filepath.Join(d, "out.txt")
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.abort() }()
	if _, err := dedup(context.Background(), dedupConfig{
		bucketPaths: []string{bucketPath},
		workers:     1,
		keepBuckets: true,
	}, sink, &Metrics{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bucketPath); err != nil {
		t.Fatalf("keepBuckets=true should retain bucket: %v", err)
	}
}

func TestChunkedZstdSinkRotatesUniqueLines(t *testing.T) {
	d := t.TempDir()
	stamp := RunStamp(time.Date(2026, 5, 10, 10, 0, 1, 0, time.UTC), "rot01a")
	sink, err := newChunkedZstdSink(d, stamp, 3, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{}
	for i := 0; i < 10; i++ {
		if err := writeLine(sink, fmt.Sprintf("line%d", i), m); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	paths := sink.outputPaths()
	if len(paths) != 4 {
		t.Fatalf("want 4 parts, got %d: %v", len(paths), paths)
	}
	var all []string
	for _, p := range paths {
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
			all = append(all, strings.Split(body, "\n")...)
		}
	}
	if len(all) != 10 {
		t.Fatalf("round-trip lines = %d, want 10: %v", len(all), all)
	}
	for _, p := range paths {
		if !strings.Contains(filepath.Base(p), "_part") {
			t.Fatalf("expected part-style name in multi-archive run, got %q", p)
		}
	}
	for i := 0; i < 10; i++ {
		want := fmt.Sprintf("line%d", i)
		if all[i] != want {
			t.Errorf("line %d = %q, want %q", i, all[i], want)
		}
	}
}

func TestChunkedZstdWriteBatchSplitsAcrossRotate(t *testing.T) {
	d := t.TempDir()
	stamp := RunStamp(time.Date(2026, 5, 10, 11, 0, 0, 0, time.UTC), "rot01b")
	sink, err := newChunkedZstdSink(d, stamp, 5, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for i := 0; i < 7; i++ {
		fmt.Fprintf(&buf, "b%d\n", i)
	}
	m := &Metrics{}
	if err := sink.writeBatch(buf.Bytes(), 7, m); err != nil {
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	paths := sink.outputPaths()
	if len(paths) != 2 {
		t.Fatalf("want 2 parts, got %d %v", len(paths), paths)
	}
	var all []string
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := zstd.NewReader(f)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		var out bytes.Buffer
		_, copyErr := io.Copy(&out, dec)
		dec.Close()
		_ = f.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		s := strings.TrimRight(out.String(), "\n")
		if s != "" {
			all = append(all, strings.Split(s, "\n")...)
		}
	}
	if len(all) != 7 {
		t.Fatalf("got %d lines: %v", len(all), all)
	}
	for _, p := range paths {
		if !strings.Contains(filepath.Base(p), "_part") {
			t.Fatalf("expected part-style names when 2+ archives, got %q", p)
		}
	}
}

func TestChunkedZstdSingleArchiveUsesDedupBasename(t *testing.T) {
	d := t.TempDir()
	stamp := RunStamp(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC), "abc123")
	sink, err := newChunkedZstdSink(d, stamp, 1000, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := writeLine(sink, fmt.Sprintf("x%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	paths := sink.outputPaths()
	if len(paths) != 1 {
		t.Fatalf("want 1 file, got %v", paths)
	}
	base := filepath.Base(paths[0])
	if strings.Contains(base, "_part") {
		t.Fatalf("single archive should not use _part suffix, got %q", base)
	}
	want := "sfu_20260510_abc123.txt.zst"
	if base != want {
		t.Fatalf("basename = %q, want %q", base, want)
	}
}

// bucketsBytesRead must equal bucketsBytesTotal after success, else
// the TUI bar reverts to chunky bucket-count fallback
func TestDedupAdvancesBucketBytes(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "https://h%d.example.com:user:p%d\n", i, i)
	}
	writeFile(t, in, b.String())

	r, err := Resolve(Config{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     filepath.Join(d, "shards"),
		Workers:     2,
		FastPathOff: true,
		Buckets:     4,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	total := m.BucketsBytesTotal.Load()
	read := m.BucketsBytesRead.Load()
	if total <= 0 {
		t.Fatalf("bucketsBytesTotal = %d, want > 0", total)
	}
	if read != total {
		t.Fatalf("bucketsBytesRead = %d, bucketsBytesTotal = %d (must match after success)", read, total)
	}
}

// -od output sidecars must index bucket hashes during dedup, not re-parse
// formatted lines (many fail parseLoose after shard formatting).
func TestDedupInlineOutputSidecar(t *testing.T) {
	d := t.TempDir()
	bucketPath := filepath.Join(d, "shard_00000.bin")
	writeBucket(t, bucketPath, []bucketRecord{
		{hash: 100, line: "host1:user:pw"},
		{hash: 200, line: "host2:user:pw"},
		{hash: 100, line: "host1:user:pw"}, // exact duplicate: dropped
	})

	out := filepath.Join(d, "sfu_test.txt.zst")
	sink, err := newOutputSinkWithSidecar(out, true, false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := dedup(context.Background(), dedupConfig{
		bucketPaths: []string{bucketPath},
		workers:     1,
	}, sink, &Metrics{})
	if err != nil {
		_ = sink.abort()
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("dedup unique lines = %d, want 2", n)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}

	hdr, err := readSidecarHeader(sidecarPathForArchive(out))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if hdr.keyCount != 2 {
		t.Fatalf("sidecar keyCount = %d, want 2 (one per unique hash)", hdr.keyCount)
	}
}

// lines with JSON-ish passwords survive shard/dedup but fail parseLoose on
// re-read. inline sidecar indexing must still capture every emitted hash.
func TestDedupInlineSidecarUnreparseableLines(t *testing.T) {
	d := t.TempDir()
	bucketPath := filepath.Join(d, "shard_00000.bin")
	recs := make([]bucketRecord, 0, 20)
	for i := 0; i < 10; i++ {
		line := fmt.Sprintf("host%d.example.com:user%d:{\"Password%d\"", i, i, i)
		recs = append(recs, bucketRecord{hash: uint64(1000 + i), line: line})
	}
	writeBucket(t, bucketPath, recs)

	out := filepath.Join(d, "sfu_test.txt.zst")
	sink, err := newOutputSinkWithSidecar(out, true, false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := dedup(context.Background(), dedupConfig{
		bucketPaths: []string{bucketPath},
		workers:     1,
	}, sink, &Metrics{})
	if err != nil {
		_ = sink.abort()
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("dedup unique lines = %d, want 10", n)
	}

	hdr, err := readSidecarHeader(sidecarPathForArchive(out))
	if err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if hdr.keyCount != 10 {
		t.Fatalf("sidecar keyCount = %d, want 10", hdr.keyCount)
	}
}

func firstDuplicate(sorted []string) string {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return sorted[i]
		}
	}
	return ""
}

// largeSidecarForTest writes a v3 sidecar with n sequential keys (all land in
// bucket 0 of a 4-bucket partition).
func largeSidecarForTest(t *testing.T, archive string, n int) {
	t.Helper()
	keys := make([]uint64, n)
	for i := range keys {
		keys[i] = uint64(i) + 1
	}
	writeSidecarKeysForTest(t, archive, keys)
}

// TestGatherCancelDuringLargeDestinationGather: a Ctrl-C during a large
// destination gather must stop the gather at a poll point and return
// ctx.Err() WITHOUT touching the persistent library sidecars.
func TestGatherCancelDuringLargeDestinationGather(t *testing.T) {
	dir := t.TempDir()
	const perSidecar = 1 << 16 // 64K keys/sidecar: a "large" gather body
	var paths []string
	for i := range 3 {
		arch := filepath.Join(dir, fmt.Sprintf("sfu_cancel_g%d.txt.zst", i))
		largeSidecarForTest(t, arch, perSidecar)
		paths = append(paths, sidecarPathForArchive(arch))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	gatherPollHook = func() {
		calls++
		if calls == 2 {
			// sidecar 1's keys are gathered; cancel before sidecar 2 opens
			cancel()
		}
	}
	defer func() { gatherPollHook = nil }()

	keys, gathered, err := gatherDestBucketKeys(ctx, paths, 0, 4, &ODMetrics{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled gather: err = %v, want context.Canceled", err)
	}
	if keys != nil || gathered != 0 {
		t.Fatalf("canceled gather returned keys=%d gathered=%d; want none", len(keys), gathered)
	}
	// persistent library sidecars must survive cancellation
	for _, p := range paths {
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("persistent sidecar %s missing after cancel: %v", p, serr)
		}
	}
}

// TestDedupCancelMidwayThroughBucket: a Ctrl-C partway through scanning one
// large bucket must abort the scan promptly, publish no output, and leave the
// persistent dest sidecar intact.
func TestDedupCancelMidwayThroughBucket(t *testing.T) {
	dir := t.TempDir()
	libArch := filepath.Join(dir, "sfu_lib.txt.zst")
	writeSidecarKeysForTest(t, libArch, []uint64{0xDEADBEEF})
	destSidecar := sidecarPathForArchive(libArch)

	bucketPath := filepath.Join(dir, defaultBucketName(0))
	const recCount = 200_000
	recs := make([]bucketRecord, 0, recCount)
	for i := range recCount {
		recs = append(recs, bucketRecord{
			hash: uint64(i) + 1, // distinct, none match the library key
			line: fmt.Sprintf("h%d.example.com:u:%d", i%64, i),
		})
	}
	writeBucket(t, bucketPath, recs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	dedupPollHook = func() {
		calls++
		if calls == 2 {
			// fires at record dedupRecordPollInterval*2: midway through the bucket
			cancel()
		}
	}
	defer func() { dedupPollHook = nil }()

	out := filepath.Join(dir, "sfu_cancel_mid.txt.zst")
	sink, serr := newOutputSinkWithSidecar(out, false, false)
	if serr != nil {
		t.Fatal(serr)
	}

	_, err := dedup(ctx, dedupConfig{
		bucketPaths:  []string{bucketPath},
		destSidecars: []string{destSidecar},
		workers:      1,
		keepBuckets:  true,
	}, sink, &Metrics{})
	if !errors.Is(err, context.Canceled) {
		_ = sink.abort()
		t.Fatalf("mid-bucket cancel: err = %v, want context.Canceled", err)
	}
	if aerr := sink.abort(); aerr != nil {
		t.Fatalf("abort: %v", aerr)
	}
	if _, serr := os.Stat(out); !os.IsNotExist(serr) {
		t.Fatalf("canceled run published output %s", out)
	}
	if _, serr := os.Stat(destSidecar); serr != nil {
		t.Fatalf("persistent dest sidecar missing after cancel: %v", serr)
	}
}

// TestGatherDestSingleBackingAllocation: the gather must hold destination keys
// in ONE ~8B/key backing slice plus bounded scratch — never the old
// raw[]byte + decoded[]uint64 + merged-output triple (peak ≈ 2-3× the budget,
// which got large libraries OS-killed).
func TestGatherDestSingleBackingAllocation(t *testing.T) {
	dir := t.TempDir()
	const perSidecar = 100_000
	var paths []string
	for i := range 4 {
		arch := filepath.Join(dir, fmt.Sprintf("sfu_alloc_%d.txt.zst", i))
		largeSidecarForTest(t, arch, perSidecar)
		paths = append(paths, sidecarPathForArchive(arch))
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	keys, gathered, err := gatherDestBucketKeys(context.Background(), paths, 0, 4, nil)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if gathered != 4*perSidecar {
		t.Fatalf("gathered = %d, want %d", gathered, 4*perSidecar)
	}
	// cross-sidecar duplicates must be compacted: sorted + strictly ascending
	for i := 1; i < len(keys); i++ {
		if keys[i] <= keys[i-1] {
			t.Fatalf("keys not sorted+unique at %d: %d then %d", i, keys[i-1], keys[i])
		}
	}

	// budget: one 8B/key backing slice + bounded read scratch + sort overhead.
	// the old shape (raw bytes + decoded runs + second merged output) peaks at
	// ~3× 8B/key and must fail this bound.
	allocDelta := after.TotalAlloc - before.TotalAlloc
	budget := uint64(gathered) * 8 * 2
	if allocDelta > budget {
		t.Fatalf("gather allocated %d bytes for %d keys (budget %d = 2×8B/key): raw+decoded+merged copies back?",
			allocDelta, gathered, budget)
	}
}

func TestGatherDestBucketKeysUpdatesKeysLoadedPerSidecar(t *testing.T) {
	dir := t.TempDir()
	arch1 := filepath.Join(dir, "sfu_a.txt.zst")
	arch2 := filepath.Join(dir, "sfu_b.txt.zst")
	writeSidecarKeysForTest(t, arch1, []uint64{1, 2, 3})
	writeSidecarKeysForTest(t, arch2, []uint64{4, 5})

	paths := []string{
		sidecarPathForArchive(arch1),
		sidecarPathForArchive(arch2),
	}
	odm := &ODMetrics{}
	_, gathered, err := gatherDestBucketKeys(context.Background(), paths, 0, 4, odm)
	if err != nil {
		t.Fatal(err)
	}
	if gathered != 5 {
		t.Fatalf("gathered = %d, want 5", gathered)
	}
	if got := odm.KeysLoaded.Load(); got != 5 {
		t.Fatalf("keysLoaded = %d, want 5 (per-sidecar ticks)", got)
	}

	// nil odm must not panic
	odm.KeysLoaded.Store(0)
	if _, _, err := gatherDestBucketKeys(context.Background(), paths, 0, 4, nil); err != nil {
		t.Fatal(err)
	}
	if odm.KeysLoaded.Load() != 0 {
		t.Fatalf("nil odm should not update keysLoaded, got %d", odm.KeysLoaded.Load())
	}
}

// TestSealAfterAbort: abort() tears down the sink's staged temps, so a later
// seal() must report a clear error (like commit-after-abort), never succeed
// or surface a confusing low-level failure. A second abort stays a no-op.
func TestSealAfterAbort(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sfu_seal_after_abort.txt")
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if aerr := sink.abort(); aerr != nil {
		t.Fatalf("abort: %v", aerr)
	}
	// idempotent abort stays a no-op
	if aerr := sink.abort(); aerr != nil {
		t.Fatalf("second abort should be a no-op, got %v", aerr)
	}
	if serr := sink.seal(); serr == nil {
		t.Fatal("seal after abort should fail, got nil")
	} else if !strings.Contains(serr.Error(), "seal after abort") {
		t.Fatalf("seal after abort: err = %v, want clear seal-after-abort error", serr)
	}
	if cerr := sink.commit(); cerr == nil {
		t.Fatal("commit after abort should fail, got nil")
	} else if !strings.Contains(cerr.Error(), "commit after abort") {
		t.Fatalf("commit after abort: err = %v, want clear commit-after-abort error", cerr)
	}
	if _, serr := os.Stat(out); !os.IsNotExist(serr) {
		t.Fatalf("aborted run published output %s", out)
	}
}
