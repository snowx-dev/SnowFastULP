package main

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cespare/xxhash/v2"
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
	m := &metrics{}
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
	if got := m.linesAccepted.Load(); got != 3 {
		t.Fatalf("metrics.linesAccepted = %d want 3", got)
	}
	if got := m.linesRejected.Load(); got != 1 {
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
	m := &metrics{}
	if _, err := shard(context.Background(), cfg, m); err != nil {
		t.Fatal(err)
	}
	if got := m.bucketsTotal.Load(); got != int64(cfg.buckets) {
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
	m := &metrics{}
	res, err := shard(context.Background(), cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	hA := xxhash.Sum64String(dedupKey("a.example.com", "user@example.com", "p"))
	hB := xxhash.Sum64String(dedupKey("b.example.com", "user@example.com", "p"))
	wantBucketA := hA & (B - 1)
	wantBucketB := hB & (B - 1)

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
	if _, err := shard(ctx, cfg, &metrics{}); err == nil {
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
	m := &metrics{}
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
	m := &metrics{}
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
