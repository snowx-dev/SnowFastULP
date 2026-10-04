package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reviewCollisionLines is the exact xxHash64 collision pair proved by the
// v0.3 deep review (C-02): two parser-valid credentials whose
// length-prefixed (host, login, password) preimages hash to the same 64-bit
// dedup key a8e9e54dbff88872.
const reviewCollisionLine1 = "a.co:u:aaaaaaa"
const reviewCollisionLine2 = "f.co:f:\xd1\x1f\xa0\xba\xbb\x7a\x1f"

// assertSameDedupKey proves the pair still collides under the current key
// rules; if the pair ever stops colliding this regression loses its premise
// and must be rebuilt against a fresh collision.
func assertSameDedupKey(t *testing.T) uint64 {
	t.Helper()
	key := func(line string) uint64 {
		host, _, login, password, ok := parseStored(line)
		if !ok {
			t.Fatalf("collision line does not parse: %q", line)
		}
		return dedupKeySum(host, login, password)
	}
	h1, h2 := key(reviewCollisionLine1), key(reviewCollisionLine2)
	if h1 != h2 {
		t.Fatalf("review pair no longer collides: %016x != %016x", h1, h2)
	}
	return h1
}

// runIngestE2E ingests one input file and returns the output text plus
// metrics. useFastPath selects the single-file fast path; false exercises the
// bucketed shard+dedup pipeline.
func runIngestE2E(t *testing.T, input string, useFastPath bool) (string, *Metrics) {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")
	tempParent := filepath.Join(dir, "stage")
	r, err := Resolve(Config{
		Inputs:      []string{input},
		Output:      outFile,
		TempDir:     tempParent,
		FastPathOff: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		UseFastPath:  useFastPath,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      tempParent,
	}
	if !useFastPath {
		res.BucketCount = 4
	}
	m := &Metrics{}
	if err := Run(context.Background(), res, m); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), m
}

// TestBucketedPipelineKeepsExactHashCollisionPair proves an exact 64-bit
// dedup-key collision between two distinct credentials no longer drops one:
// both records must reach the output.
func TestBucketedPipelineKeepsExactHashCollisionPair(t *testing.T) {
	assertSameDedupKey(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	body := reviewCollisionLine1 + "\n" + reviewCollisionLine2 + "\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, m := runIngestE2E(t, src, false)
	if m.LinesAccepted.Load() != 2 {
		t.Fatalf("LinesAccepted = %d, want 2", m.LinesAccepted.Load())
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output has %d lines, want 2 (distinct credentials must survive an exact key collision): %q", len(lines), lines)
	}
	if !strings.Contains(out, "aaaaaaa") || !strings.Contains(out, "\xd1\x1f\xa0\xba\xbb\x7a\x1f") {
		t.Fatalf("one collision credential is missing from output: %q", out)
	}
}

// TestFastPathKeepsExactHashCollisionPair is the same regression for the
// single-file fast path, which dedups with its own seen map.
func TestFastPathKeepsExactHashCollisionPair(t *testing.T) {
	assertSameDedupKey(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.txt")
	body := reviewCollisionLine1 + "\n" + reviewCollisionLine2 + "\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, m := runIngestE2E(t, src, true)
	if m.LinesAccepted.Load() != 2 {
		t.Fatalf("LinesAccepted = %d, want 2", m.LinesAccepted.Load())
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output has %d lines, want 2 (distinct credentials must survive an exact key collision): %q", len(lines), lines)
	}
}

// TestDedupStageKeepsExactHashCollisionPair drives the dedup stage directly
// with the pair's REAL engine-computed key (no forced hash), so the regression
// pins both the key collision and the strong-identity rescue.
func TestDedupStageKeepsExactHashCollisionPair(t *testing.T) {
	h := assertSameDedupKey(t)
	d := t.TempDir()
	bucketPath := filepath.Join(d, defaultBucketName(0))
	writeBucket(t, bucketPath, []bucketRecord{
		{hash: h, line: reviewCollisionLine1},
		{hash: h, line: reviewCollisionLine2},
	})
	out := filepath.Join(d, "out.txt")
	sink, err := newOutputSink(out, false, false)
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
	if n != 2 {
		t.Fatalf("want 2 unique records under an exact key collision, got %d", n)
	}
	got := readLines(t, out)
	if len(got) != 2 || got[0] != reviewCollisionLine1 || got[1] != reviewCollisionLine2 {
		t.Fatalf("output = %q, want both collision credentials in first-seen order", got)
	}
}

// TestDedupDropsExactDuplicateRecord keeps the true-duplicate fast path: a
// record whose line (and therefore key preimage) is byte-identical is still a
// dup and must be dropped.
func TestDedupDropsExactDuplicateRecord(t *testing.T) {
	h := assertSameDedupKey(t)
	d := t.TempDir()
	bucketPath := filepath.Join(d, defaultBucketName(0))
	writeBucket(t, bucketPath, []bucketRecord{
		{hash: h, line: reviewCollisionLine1},
		{hash: h, line: reviewCollisionLine1}, // exact duplicate: dropped
		{hash: h + 1, line: reviewCollisionLine2 + "-x"},
	})
	out := filepath.Join(d, "out.txt")
	sink, err := newOutputSink(out, false, false)
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
	if n != 2 {
		t.Fatalf("want 2 unique records (exact duplicate dropped), got %d", n)
	}
}
