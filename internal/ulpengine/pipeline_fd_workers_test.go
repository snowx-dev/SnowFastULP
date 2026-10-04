//go:build unix

package ulpengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fdWorkerInputs builds n one-chunk input files with parseable credentials
// and returns their paths. Big enough that every worker is mid-read while
// the bucket writers are open, which is when the FD budget binds.
func fdWorkerInputs(t *testing.T, dir string, n, lines int) []string {
	t.Helper()
	paths := make([]string, 0, n)
	for i := range n {
		var b strings.Builder
		for j := 0; j < lines; j++ {
			fmt.Fprintf(&b, "h%d.example.com:u%d_%d:p\n", i, i, j)
		}
		p := filepath.Join(dir, fmt.Sprintf("in_%03d.txt", i))
		if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

// TestShardFDLimitAccountsForWorkerInputs: Resolve's FD budget must include
// the one input descriptor each concurrent parser worker holds during phase
// 1 (M-07). Under RLIMIT 144, 64 bucket writers plus 80 worker-held input
// fds exceed the limit with even one process fd to spare, so the old budget
// (maxFD - reserve only, workers not subtracted) accepted the setup and
// sharding then failed late with "too many open files". The fixed budget
// clamps workers jointly with buckets and the same run completes.
func TestShardFDLimitAccountsForWorkerInputs(t *testing.T) {
	lowerFDLimit(t, 144)

	dir := t.TempDir()
	const workers = 80
	inputs := fdWorkerInputs(t, dir, workers, 10_000)

	r, err := Resolve(Config{
		Inputs:      inputs,
		Output:      filepath.Join(dir, "out.txt"),
		TempDir:     filepath.Join(dir, "stage"),
		Workers:     workers,
		Buckets:     64,
		FastPathOff: true,
		RunStarted:  time.Date(2026, 5, 10, 15, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Resolve under RLIMIT 144: %v", err)
	}
	// 144 - 32 reserve - 64 min buckets = 48 parser workers fit alongside
	// the 64 bucket writers; the pre-fix budget kept all 80.
	if want := 144 - fdReserve - minBuckets; r.Workers != want {
		t.Errorf("Resolve.Workers = %d, want %d (fd-clamped jointly with buckets)", r.Workers, want)
	}

	m := &Metrics{}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatalf("run under RLIMIT 144: %v", err)
	}
	if got := m.LinesUnique.Load(); got != workers*10_000 {
		t.Errorf("LinesUnique = %d, want %d", got, workers*10_000)
	}
}

// TestResolveFDLimitTooLowWithWorkers: with workers in the budget, a soft
// limit that cannot fit minBuckets + one worker input descriptor must be
// rejected up front instead of being accepted and failing mid-shard.
func TestResolveFDLimitTooLowWithWorkers(t *testing.T) {
	lowerFDLimit(t, 96) // 96 - 32 reserve = 64 = minBuckets only, no worker fd

	dir := t.TempDir()
	inputs := fdWorkerInputs(t, dir, 2, 10)

	_, err := Resolve(Config{
		Inputs:      inputs,
		Output:      filepath.Join(dir, "out.txt"),
		TempDir:     filepath.Join(dir, "stage"),
		Workers:     8,
		Buckets:     64,
		FastPathOff: true,
		RunStarted:  time.Date(2026, 5, 10, 15, 4, 5, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("Resolve accepted a limit that cannot fit minBuckets + one worker input fd")
	}
	if !strings.Contains(err.Error(), "file descriptor ulimit") {
		t.Errorf("error = %v, want the fd ulimit guidance message", err)
	}
}
