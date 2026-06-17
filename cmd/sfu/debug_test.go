package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDebugArtifactPathCollision(t *testing.T) {
	d := t.TempDir()
	stamp := runStamp(time.Date(2020, 1, 2, 15, 4, 5, 0, time.UTC), "test01")
	p1, err := debugArtifactPath(d, "sfu-debug", ".log", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p1, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, err := debugArtifactPath(d, "sfu-debug", ".log", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("expected distinct paths, got %s", p1)
	}
}

func TestDebugRejectBucketed(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com/p1:user@example.com:p1\n"+
			"not-a-line\n"+
			"https://b.example.com:user2:p2\n",
	)
	rejPath := filepath.Join(d, "rej.txt")
	rr, err := newRejectRecorder(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rr.Close() })

	cfg := pipelineConfig{
		Inputs:       []string{in},
		Output:       filepath.Join(d, "out.txt"),
		TempDir:      filepath.Join(d, "shards"),
		Workers:      4,
		DedupWorkers: 2,
		Buckets:      8,
		ChunkBytes:   1 << 20,
		FastPathOff:  true,
		Reject:       rr,
	}
	r, err := resolvePipelineConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &metrics{totalInputBytes: r.totalInputs}
	if err := run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	_ = rr.Close()
	raw, err := os.ReadFile(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "not-a-line") {
		t.Fatalf("reject file missing bad line: %q", s)
	}
	if !strings.Contains(s, in) {
		t.Fatalf("reject file missing input path: %q", s)
	}
	if m.linesRejected.Load() != 1 {
		t.Fatalf("linesRejected = %d", m.linesRejected.Load())
	}
}

func TestDebugRejectFastPath(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in,
		"https://a.example.com:user:p\n"+
			"totally-not-ulP\n",
	)
	rejPath := filepath.Join(d, "rej.txt")
	rr, err := newRejectRecorder(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rr.Close() })

	cfg := pipelineConfig{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		TempDir: filepath.Join(d, "shards"),
		Workers: 1,
		Reject:  rr,
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
	_ = rr.Close()
	raw, err := os.ReadFile(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "totally-not-ulP") {
		t.Fatalf("reject file: %q", s)
	}
	if !strings.Contains(s, "\t2\t") {
		t.Fatalf("expected 1-based line ref 2, got: %q", s)
	}
}

func TestDebugLogRationaleAndCompletionDetail(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "https://a.example.com:user:p\n")
	logPath := filepath.Join(d, "dbg.log")
	dbg, err := newDebugLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbg.Close() })

	cfg := pipelineConfig{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     filepath.Join(d, "shards"),
		Workers:     1,
		FastPathOff: true,
		Buckets:     100, // 100 rounds up to 128
		Debug:       dbg,
	}
	r, err := resolvePipelineConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbg.writeHeader("sfu", time.Now(), []string{"sfu", in}, []string{in}, r)
	dbg.logResolutionRationale(r)
	m := &metrics{totalInputBytes: r.totalInputs}
	if err := run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	dbg.logCompletion(m, time.Second, r)
	_ = dbg.Close()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	wantSubs := []string{
		"Resolution rationale",
		"mem:",
		"fastPath:",
		"bucketCount: 128 (user (rounded up))",
		"sink: plain text",
		"[event +", "runDir:",
		"outputPaths:",
		"on disk)",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in debug log:\n%s", s, out)
		}
	}
}

// one [event] per rotation, plus the part1 rename
func TestChunkedZstdSinkLogsRotationEvents(t *testing.T) {
	d := t.TempDir()
	logPath := filepath.Join(d, "dbg.log")
	dbg, err := newDebugLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbg.Close() })
	stamp := runStamp(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC), "rot001")
	sink, err := newChunkedZstdSink(d, stamp, 2, dbg, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// 5 lines, chunkLines=2 => 2+2+1 = 3 archives, 2 rotations
	for i := 0; i < 5; i++ {
		if err := sink.writeLine("x", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.close(); err != nil {
		t.Fatal(err)
	}
	_ = dbg.Close()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, "rotate-rename: part=1") {
		t.Errorf("missing rotate-rename event:\n%s", out)
	}
	if !strings.Contains(out, "rotate-open: part=2") {
		t.Errorf("missing rotate-open part=2 event:\n%s", out)
	}
	if !strings.Contains(out, "rotate-open: part=3") {
		t.Errorf("missing rotate-open part=3 event:\n%s", out)
	}
}

func TestDebugLogWritesHeaderAndPhases(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "https://a.example.com:user:p\n")
	logPath := filepath.Join(d, "dbg.log")
	dbg, err := newDebugLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbg.Close() })

	cfg := pipelineConfig{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     filepath.Join(d, "shards"),
		Workers:     1,
		FastPathOff: true,
		Buckets:     4,
		Debug:       dbg,
	}
	r, err := resolvePipelineConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbg.writeHeader("sfu", time.Now(), []string{"sfu", in}, []string{in}, r)
	m := &metrics{totalInputBytes: r.totalInputs}
	if err := run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	_ = dbg.Close()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, "Resolved pipeline") {
		t.Fatalf("missing header sections: %q", out)
	}
	if !strings.Contains(out, "PHASE shard START") || !strings.Contains(out, "PHASE dedup END") {
		t.Fatalf("missing phase markers: %q", out)
	}
}
