package ulpengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestRunDropsEmptyOutputAllRejected proves an end-to-end run whose every input
// line is rejected leaves no output shard on disk and clears OutputPaths, on
// both the bucketed and fast paths (so sfu -o/-od and sfl -od all stay tidy).
// The transactional variant (a pre-existing output survives an empty run) is
// covered by TestEmptyRunKeepsExistingOutput in output_atomic_test.go.
func TestRunDropsEmptyOutputAllRejected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fastPath bool
	}{
		{"bucketed", false},
		{"fastpath", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			in := filepath.Join(d, "in.txt")
			writeFile(t, in, "not-a-line\nalso not valid\n\n")
			out := filepath.Join(d, "out.txt")
			cfg := Config{
				Inputs:       []string{in},
				Output:       out,
				TempDir:      filepath.Join(d, "shards"),
				Workers:      2,
				DedupWorkers: 2,
				Buckets:      8,
				ChunkBytes:   1 << 20,
				FastPathOff:  !tc.fastPath,
			}
			r, err := Resolve(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r.UseFastPath = tc.fastPath // force the path deterministically
			m := &Metrics{TotalInputBytes: r.TotalInputs}
			if err := Run(context.Background(), r, m); err != nil {
				t.Fatal(err)
			}
			if m.LinesUnique.Load() != 0 {
				t.Fatalf("LinesUnique = %d, want 0", m.LinesUnique.Load())
			}
			if len(r.OutputPaths) != 0 {
				t.Fatalf("OutputPaths = %v, want empty (shard discarded)", r.OutputPaths)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("empty output should be removed; stat err=%v", err)
			}
		})
	}
}

// TestRunEmptyInputShortCircuitsLibrary pins RR-3.7: when every selected input
// is empty, the run must succeed with no output and must NOT touch the library
// at all — no lock file, no sidecar discovery/scan, no dest-key gather, no
// buckets — so a no-op ingest costs nothing regardless of library size.
func TestRunEmptyInputShortCircuitsLibrary(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	writeFile(t, in, "") // zero bytes

	// fake library: several archives with sizable sidecars
	lib := filepath.Join(d, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		arch := filepath.Join(lib, fmt.Sprintf("sfu_20260101_%02d.txt.zst", i))
		if err := os.WriteFile(arch, []byte("dummy archive bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		keys := make([]uint64, 50_000)
		for j := range keys {
			keys[j] = uint64(i)*1_000_000 + uint64(j)
		}
		writeSidecarKeysForTest(t, arch, keys)
	}

	// seam counter: every library archive stat go through statArchive
	orig := statArchive
	var statCalls int64
	statArchive = func(p string) (os.FileInfo, error) {
		atomic.AddInt64(&statCalls, 1)
		return orig(p)
	}
	t.Cleanup(func() { statArchive = orig })

	out := filepath.Join(d, "out.txt")
	r, err := Resolve(Config{
		Inputs:       []string{in},
		Output:       out,
		TempDir:      filepath.Join(d, "tmp"),
		Workers:      2,
		DedupWorkers: 2,
		DestDedup:    true,
		DestDedupDir: lib,
		RunStamp:     "emptyrun",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := &Metrics{TotalInputBytes: r.TotalInputs}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatalf("empty-input run: %v", err)
	}

	if statCalls != 0 {
		t.Fatalf("library touched %d time(s) for an all-empty-input run; want 0 archive/sidecar stats", statCalls)
	}
	if _, err := os.Stat(filepath.Join(lib, libraryLockFileName)); !os.IsNotExist(err) {
		t.Fatalf("library lock created for a no-op run: %v", err)
	}
	if len(r.OutputPaths) != 0 {
		t.Fatalf("OutputPaths = %v, want empty", r.OutputPaths)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("no output expected for zero unique lines; stat err=%v", err)
	}
	if m.Phase.Load() != phaseDone {
		t.Fatalf("phase = %d, want phaseDone", m.Phase.Load())
	}
}

// TestChooseBucketCountAuxFloorAtZeroInput pins defense in depth: even at zero
// input bytes the -od auxiliary dest-key sizing must still raise B, so a
// pipeline that ever reaches the sizer with empty input stays library-bounded.
func TestChooseBucketCountAuxFloorAtZeroInput(t *testing.T) {
	mem := memInfo{} // unknown memory → target defaults
	if got := chooseBucketCount(0, 0, mem, 4, minBuckets, maxBuckets); got != minBuckets {
		t.Fatalf("zero input, no aux: B = %d, want minB (%d)", got, minBuckets)
	}
	// 64 GiB of dest keys / 128 MiB per bucket = 512
	if got := chooseBucketCount(0, 64<<30, mem, 4, minBuckets, maxBuckets); got != 512 {
		t.Fatalf("zero input with 64 GiB aux keys: B = %d, want 512 (aux floor applied)", got)
	}
}
