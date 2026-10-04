package ulpengine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// returns every (hash, line) record in bucket file at p
func readBucket(t *testing.T, p string) []bucketRecord {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []bucketRecord
	var hdr [bucketRecordHeaderBytes]byte
	for {
		_, err := io.ReadFull(f, hdr[:])
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		h := binary.LittleEndian.Uint64(hdr[0:8])
		n := binary.LittleEndian.Uint32(hdr[8:12])
		buf := make([]byte, n)
		if _, err := io.ReadFull(f, buf); err != nil {
			t.Fatal(err)
		}
		out = append(out, bucketRecord{hash: h, line: string(buf)})
	}
}

type bucketRecord struct {
	hash uint64
	line string
}

func TestShardRoundTripSingleBucket(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com/p1:user@example.com:p1\n"+
			"https://a.example.com/p2:user@example.com:p1\n"+ // same dedup key
			"not-a-line\n"+
			"https://b.example.com:user2:p2\n",
	)

	tmp := filepath.Join(d, "shards")
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    tmp,
		buckets:    1,
		workers:    1,
		chunkBytes: 1 << 20,
	}
	m := &Metrics{}
	res, err := shard(context.Background(), cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.bucketPaths) != 1 {
		t.Fatalf("want 1 bucket path, got %d", len(res.bucketPaths))
	}
	recs := readBucket(t, res.bucketPaths[0])
	if len(recs) != 3 {
		t.Fatalf("want 3 valid records (one rejected), got %d: %v", len(recs), recs)
	}
	if got := m.LinesAccepted.Load(); got != 3 {
		t.Fatalf("metrics.linesAccepted = %d want 3", got)
	}
	if got := m.LinesRejected.Load(); got != 1 {
		t.Fatalf("metrics.linesRejected = %d want 1", got)
	}
}

// bucketsTotal must be non-zero by shard() return. regression for
// -debug [progress] logging buckets=0/0 during phase 1
func TestShardPublishesBucketsTotalEarly(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "https://a.example.com:user:p\n")
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    filepath.Join(d, "shards"),
		buckets:    4,
		workers:    1,
		chunkBytes: 1 << 20,
	}
	m := &Metrics{}
	if _, err := shard(context.Background(), cfg, m); err != nil {
		t.Fatal(err)
	}
	if got := m.BucketsTotal.Load(); got != int64(cfg.buckets) {
		t.Fatalf("bucketsTotal = %d, want %d", got, cfg.buckets)
	}
}

func TestShardKeyRoutesToCorrectBucket(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com/x:user@example.com:p\n"+
			"https://b.example.com/x:user@example.com:p\n",
	)

	tmp := filepath.Join(d, "shards")
	const B = 8
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    tmp,
		buckets:    B,
		workers:    2,
		chunkBytes: 1 << 20,
	}
	m := &Metrics{}
	res, err := shard(context.Background(), cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	hA := dedupKeySum("a.example.com", "user@example.com", "p")
	hB := dedupKeySum("b.example.com", "user@example.com", "p")
	// top-bits range partition (must match shard's bucketIndex)
	wantBucketA := bucketIndex(hA, B-1, true, B)
	wantBucketB := bucketIndex(hB, B-1, true, B)

	for i, p := range res.bucketPaths {
		recs := readBucket(t, p)
		switch {
		case uint64(i) == wantBucketA:
			if len(recs) == 0 {
				t.Fatalf("bucket %d should contain key A", i)
			}
		case uint64(i) == wantBucketB:
			if len(recs) == 0 {
				t.Fatalf("bucket %d should contain key B", i)
			}
		default:
			if len(recs) != 0 {
				t.Fatalf("bucket %d should be empty, has %d records", i, len(recs))
			}
		}
	}
}

func TestShardReturnsErrorOnCanceledContext(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	// enough lines that workers dont finish before cancel
	var b strings.Builder
	for i := 0; i < 50_000; i++ {
		b.WriteString("https://h.example.com/p")
		b.WriteString(strings.Repeat("x", 16))
		b.WriteString(":user:p\n")
	}
	writeFile(t, in, b.String())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    filepath.Join(d, "shards"),
		buckets:    4,
		workers:    2,
		chunkBytes: 1024,
	}
	if _, err := shard(ctx, cfg, &Metrics{}); err == nil {
		t.Fatal("shard with pre-canceled context should return non-nil error")
	}
}

func TestShardKeepsLineWhenChunkAlignsOnNewline(t *testing.T) {
	// chunkBytes=10 = boundary on every '\n'. prev logic dropped the
	// first line of every non-zero chunk
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	const lineCount = 8
	var b strings.Builder
	for i := 0; i < lineCount; i++ {
		// "x.com:u:p\n" = 10 bytes
		b.WriteString("x.com:u:p\n")
	}
	writeFile(t, in, b.String())

	tmp := filepath.Join(d, "shards")
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    tmp,
		buckets:    1,
		workers:    2,
		chunkBytes: 10, // each chunk = one line
	}
	m := &Metrics{}
	res, err := shard(context.Background(), cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	recs := readBucket(t, res.bucketPaths[0])
	// all lines same key, bucket has lineCount records (dedup in phase 2)
	if len(recs) != lineCount {
		t.Fatalf("got %d records, want %d (chunk boundary dropped lines)", len(recs), lineCount)
	}
}

func TestShardSurvivesChunkBoundary(t *testing.T) {
	// tiny chunkBytes splits mid-line, readers must drop partial first
	// line and pick up the next full line crossing their range
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com:user1:p1\n"+
			"https://b.example.com:user2:p2\n"+
			"https://c.example.com:user3:p3\n"+
			"https://d.example.com:user4:p4\n",
	)
	tmp := filepath.Join(d, "shards")
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    tmp,
		buckets:    4,
		workers:    4,
		chunkBytes: 16, // splits mid-line
	}
	m := &Metrics{}
	res, err := shard(context.Background(), cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range res.bucketPaths {
		total += len(readBucket(t, p))
	}
	if total != 4 {
		t.Fatalf("want 4 records across buckets, got %d", total)
	}
}

// TestShardBytesReadExactLongLineStraddle covers the production over-count regime:
// a long line (longer than maxInputLineBytes, so readBoundedLine drains it as
// one tooLong record) that straddles a chunk boundary. Without the in-range
// clamp the chunk credits the full line AND the next chunk's skipPartial
// re-credits the tail; with the clamp the chunk credits only its in-range
// slice and the next chunk owns the tail exactly once, so BytesRead == file size.
func TestShardBytesReadExactLongLineStraddle(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")

	// 90 KiB of 10-byte "x.com:u:p\n" lines, then one 50 KiB line with no
	// '\n' until the end, then '\n'. The 50 KiB line starts at 90 KiB and ends
	// at ~140 KiB, straddling the 100 KiB chunk boundary.
	const smallLine = "x.com:u:p\n"
	const smallCount = 90 * 1024 / 10 // 92160 B / 10 = 9216 lines
	var b strings.Builder
	for i := 0; i < smallCount; i++ {
		b.WriteString(smallLine)
	}
	b.WriteString(strings.Repeat("x", 50*1024)) // 50 KiB, no newline
	b.WriteByte('\n')
	content := b.String()

	writeFile(t, in, content)

	tmp := filepath.Join(d, "shards")
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    tmp,
		buckets:    1,
		workers:    2,
		chunkBytes: 100 * 1024, // > the long line, so it straddles exactly one boundary
	}
	m := &Metrics{}
	if _, err := shard(context.Background(), cfg, m); err != nil {
		t.Fatal(err)
	}
	if got := m.BytesRead.Load(); got != int64(len(content)) {
		t.Fatalf("BytesRead = %d, want %d (file size); long-line straddle over-counted by %d",
			got, int64(len(content)), got-int64(len(content)))
	}
}

// TestShardBytesReadExactMultiChunkLongLine covers the multi-chunk regime
// (lineLen > chunkBytes): a single long line spans many chunks, and every
// subsequent chunk's skipPartial re-credits the remaining tail. This is the
// case that proves the clamp must apply to the skipPartial path too, not only
// the normal last line. Without the fix this over-counts by ~12.5x.
func TestShardBytesReadExactMultiChunkLongLine(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")

	// One 100 KiB line with no '\n' until the end, then '\n'. With 4 KiB
	// chunks the line spans 25 chunks.
	content := strings.Repeat("x", 100*1024) + "\n"
	writeFile(t, in, content)

	tmp := filepath.Join(d, "shards")
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    tmp,
		buckets:    1,
		workers:    2,
		chunkBytes: 4 * 1024, // line spans many chunks
	}
	m := &Metrics{}
	if _, err := shard(context.Background(), cfg, m); err != nil {
		t.Fatal(err)
	}
	if got := m.BytesRead.Load(); got != int64(len(content)) {
		t.Fatalf("BytesRead = %d, want %d (file size); multi-chunk long line over-counted by %d",
			got, int64(len(content)), got-int64(len(content)))
	}
}

// TestShardBytesReadUTF16Unchanged is a regression guard for the
// `counter == nil` clamp guard: UTF-16 jobs are whole-file (no straddle
// and use raw-byte counter deltas vs decoded offsets (unit mismatch), so the
// clamp is skipped. BytesRead must still equal the raw file size minus BOM,
// and the record must parse through to a bucket.
func TestShardBytesReadUTF16Unchanged(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")

	// UTF-16 LE w/ BOM, same construction as encoding_test.go:75.
	const line = "https://h.example.com:u:p\n"
	var raw bytes.Buffer
	raw.Write([]byte{0xff, 0xfe}) // UTF-16 LE BOM
	for _, ch := range line {
		raw.WriteByte(byte(ch))
		raw.WriteByte(0)
	}
	if err := os.WriteFile(in, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	tmp := filepath.Join(d, "shards")
	cfg := shardConfig{
		inputs:     []string{in},
		tempDir:    tmp,
		buckets:    1,
		workers:    1,
		chunkBytes: 1 << 20, // > file size -> whole-file UTF-16 job
	}
	m := &Metrics{}
	res, err := shard(context.Background(), cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	// BOM (2 bytes) is discarded before the counter wraps the file, so
	// BytesRead == raw bytes after BOM.
	wantRead := int64(raw.Len()) - 2
	if got := m.BytesRead.Load(); got != wantRead {
		t.Fatalf("BytesRead = %d, want %d (file size minus 2-byte BOM); UTF-16 accounting changed",
			got, wantRead)
	}
	// And the credential still parses through to the bucket.
	total := 0
	for _, p := range res.bucketPaths {
		total += len(readBucket(t, p))
	}
	if total != 1 {
		t.Fatalf("want 1 record across buckets, got %d", total)
	}
}

// TestBuildChunkJobsHugeLineSingleJob pins RR-3.6: a newline-free file many
// times chunkBytes must produce ONE effective job (the huge record is drained
// once during boundary alignment), not one job per nominal chunk whose
// skipPartial re-drains the record each time.
func TestBuildChunkJobsHugeLineSingleJob(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "huge.txt")
	chunk := int64(1 << 20)
	body := bytes.Repeat([]byte("A"), int(4*chunk)+7) // no '\n' anywhere
	writeFile(t, p, string(body))

	jobs, err := buildChunkJobs(context.Background(), []string{p}, chunk, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 (huge line drained once, not once per nominal chunk)", len(jobs))
	}
	if jobs[0].start != 0 || jobs[0].end != int64(len(body)) {
		t.Fatalf("job range = [%d,%d), want [0,%d)", jobs[0].start, jobs[0].end, len(body))
	}
}

// countingSeeker wraps a ReadSeeker and counts consumed source bytes so tests
// can assert alignment I/O is O(file length), not O(chunks x record).
type countingSeeker struct {
	r io.ReadSeeker
	n int64
}

func (c *countingSeeker) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingSeeker) Seek(off int64, whence int) (int64, error) {
	return c.r.Seek(off, whence)
}

// TestBuildChunkJobsHugeLineAlignmentReadsLinearly instruments the alignment
// reader: a newline-free file 5x chunkBytes must be drained ~once (O(size)
// bytes), never once per nominal chunk.
func TestBuildChunkJobsHugeLineAlignmentReadsLinearly(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "huge.txt")
	chunk := int64(1 << 20)
	body := bytes.Repeat([]byte("A"), int(5*chunk)+7)
	writeFile(t, p, string(body))

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cs := &countingSeeker{r: f}

	jobs, err := buildFileChunkJobs(context.Background(), cs, p, int64(len(body)), chunk, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if cs.n > int64(len(body)) {
		t.Fatalf("alignment read %d bytes for a %d-byte file; want one drain (O(size)), got O(chunks x record)", cs.n, len(body))
	}
}

// TestBuildChunkJobsJobsOnRecordBoundaries pins that generated jobs are exact
// line-boundary ranges (start at 0 or just past a '\n'; ends likewise), cover
// the file contiguously, and that a huge line in the middle is drained once.
func TestBuildChunkJobsJobsOnRecordBoundaries(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "mixed.txt")
	chunk := int64(1 << 20)

	var b bytes.Buffer
	b.WriteString(strings.Repeat("x.com:u:p\n", 100)) // 1000 B of normal lines
	b.Write(bytes.Repeat([]byte("A"), int(3*chunk)))
	b.WriteByte('\n')
	b.WriteString(strings.Repeat("y.com:u:p\n", 50))
	body := b.Bytes()
	writeFile(t, p, string(body))

	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cs := &countingSeeker{r: f}
	jobs, err := buildFileChunkJobs(context.Background(), cs, p, int64(len(body)), chunk, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	prevEnd := int64(0)
	for i, j := range jobs {
		if j.start != prevEnd {
			t.Fatalf("job %d start %d, want %d (contiguous)", i, j.start, prevEnd)
		}
		prevEnd = j.end
		if j.start > 0 && body[j.start-1] != '\n' {
			t.Fatalf("job %d starts mid-record at %d", i, j.start)
		}
		if j.end < int64(len(body)) && body[j.end-1] != '\n' {
			t.Fatalf("job %d ends mid-record at %d", i, j.end)
		}
	}
	if prevEnd != int64(len(body)) {
		t.Fatalf("last end %d, want file size %d", prevEnd, len(body))
	}
	// huge line drained exactly once: total alignment reads stay linear
	if cs.n > 2*int64(len(body)) {
		t.Fatalf("alignment read %d bytes for a %d-byte file; want O(size)", cs.n, len(body))
	}
}

// TestBuildChunkJobsCanceledContext pins ctx polling during alignment: a
// canceled context stops job construction instead of draining a huge record.
func TestBuildChunkJobsCanceledContext(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "huge.txt")
	chunk := int64(1 << 20)
	body := bytes.Repeat([]byte("A"), int(5*chunk))
	writeFile(t, p, string(body))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := buildChunkJobs(ctx, []string{p}, chunk, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
