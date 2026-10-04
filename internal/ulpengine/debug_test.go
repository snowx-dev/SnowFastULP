package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCreateArtifactFileCollision(t *testing.T) {
	d := t.TempDir()
	stamp := RunStamp(time.Date(2020, 1, 2, 15, 4, 5, 0, time.UTC), "test01")
	f1, p1, err := CreateArtifactFile(d, "sfu-debug-"+stamp, ".log", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f1.Close()
	f2, p2, err := CreateArtifactFile(d, "sfu-debug-"+stamp, ".log", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = f2.Close()
	if p1 == p2 {
		t.Fatalf("expected distinct paths, got %s", p1)
	}
	if filepath.Base(p2) != "sfu-debug-"+stamp+"_2.log" {
		t.Fatalf("path = %q", p2)
	}
}

// The create must live inside the retry loop: two racers with the same base
// used to both Stat-miss on a prospective path and then both open it with
// O_TRUNC, clobbering one run's log. Exclusive creation hands each racer its
// own file (the loser of the first name gets _2).
func TestCreateArtifactFileConcurrent(t *testing.T) {
	d := t.TempDir()
	stamp := RunStamp(time.Date(2020, 1, 2, 15, 4, 5, 0, time.UTC), "test01")
	const racers = 8
	paths := make([]string, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, p, err := CreateArtifactFile(d, "sfu-debug-"+stamp, ".log", 0o600)
			if err != nil {
				t.Error(err)
				return
			}
			paths[i] = p
			_ = f.Close()
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, racers)
	for _, p := range paths {
		if p == "" {
			t.Fatal("a racer returned no path")
		}
		if seen[p] {
			t.Fatalf("two racers share the path %s", p)
		}
		seen[p] = true
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if !seen[filepath.Join(d, "sfu-debug-"+stamp+".log")] ||
		!seen[filepath.Join(d, "sfu-debug-"+stamp+"_2.log")] {
		t.Fatalf("expected the plain and _2 names among %v", paths)
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
	rr, err := NewRejectRecorder(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rr.Close() })

	cfg := Config{
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
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
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
	if m.LinesRejected.Load() != 1 {
		t.Fatalf("linesRejected = %d", m.LinesRejected.Load())
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
	rr, err := NewRejectRecorder(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rr.Close() })

	cfg := Config{
		Inputs:  []string{in},
		Output:  filepath.Join(d, "out.txt"),
		TempDir: filepath.Join(d, "shards"),
		Workers: 1,
		Reject:  rr,
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
	dbg, err := NewDebugLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbg.Close() })

	cfg := Config{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     filepath.Join(d, "shards"),
		Workers:     1,
		FastPathOff: true,
		Buckets:     100, // 100 rounds up to 128
		Debug:       dbg,
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbg.WriteHeader("sfu", time.Now(), []string{"sfu", in}, []string{in}, r)
	dbg.LogResolutionRationale(r)
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatal(err)
	}
	dbg.LogCompletion(m, time.Second, r)
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
	dbg, err := NewDebugLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbg.Close() })
	stamp := RunStamp(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC), "rot001")
	sink, err := newChunkedZstdSink(d, stamp, 2, dbg, false, false)
	if err != nil {
		t.Fatal(err)
	}
	// 5 lines, chunkLines=2 => 2+2+1 = 3 archives, 2 rotations
	for i := 0; i < 5; i++ {
		if err := writeLine(sink, "x", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
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
	dbg, err := NewDebugLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbg.Close() })

	cfg := Config{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     filepath.Join(d, "shards"),
		Workers:     1,
		FastPathOff: true,
		Buckets:     4,
		Debug:       dbg,
	}
	r, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbg.WriteHeader("sfu", time.Now(), []string{"sfu", in}, []string{in}, r)
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
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

// Flush must land buffered reject lines on disk without closing: process
// exit seams (fatal/interrupt/force-exit) flush through the termctl hook
// because os.Exit skips the deferred Close, so the tail an abnormal exit
// exists to diagnose must survive a bare Flush. Close stays idempotent
// afterwards, and a Flush after Close is a silent no-op, never a panic.
func TestRejectRecorderFlushPersistsWithoutClose(t *testing.T) {
	d := t.TempDir()
	rejPath := filepath.Join(d, "rej.txt")
	rr, err := NewRejectRecorder(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	rr.Record("/abs/in.txt", "1", "not-a-line")
	rr.RecordTagged("/abs/in.txt", "2", "password>64", "https://h.example.com:u:p")
	rr.Flush()

	raw, err := os.ReadFile(rejPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "not-a-line") || !strings.Contains(s, "[password>64]") {
		t.Fatalf("flushed file missing buffered lines: %q", s)
	}

	if err := rr.Close(); err != nil {
		t.Fatalf("close after flush: %v", err)
	}
	if err := rr.Close(); err != nil {
		t.Fatalf("close not idempotent after flush: %v", err)
	}
	rr.Flush() // after Close: silent, must not panic
	var nilRR *RejectRecorder
	nilRR.Flush() // nil receiver: silent no-op
}
