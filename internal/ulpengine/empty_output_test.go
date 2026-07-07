package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDiscardEmptyOutputRemovesShardAndSidecars proves a 0-line run drops the
// archive plus its .idx and search sidecars and reports no surviving paths.
func TestDiscardEmptyOutputRemovesShardAndSidecars(t *testing.T) {
	d := t.TempDir()
	arch := filepath.Join(d, "sfu_x.txt.zst")
	files := []string{arch, sidecarPathForArchive(arch), searchSidecarPathForArchive(arch)}
	for _, p := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := discardEmptyOutput(&Metrics{}, []string{arch}); got != nil {
		t.Fatalf("returned paths %v, want nil", got)
	}
	for _, p := range files {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, stat err=%v", filepath.Base(p), err)
		}
	}
}

// TestDiscardEmptyOutputKeepsWhenLinesWritten proves a run that wrote lines is
// untouched.
func TestDiscardEmptyOutputKeepsWhenLinesWritten(t *testing.T) {
	d := t.TempDir()
	arch := filepath.Join(d, "sfu_x.txt.zst")
	if err := os.WriteFile(arch, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Metrics{}
	m.LinesUnique.Store(3)
	got := discardEmptyOutput(m, []string{arch})
	if len(got) != 1 || got[0] != arch {
		t.Fatalf("returned %v, want [%s]", got, arch)
	}
	if _, err := os.Stat(arch); err != nil {
		t.Fatalf("archive should survive: %v", err)
	}
}

// TestRunDropsEmptyOutputAllRejected proves an end-to-end run whose every input
// line is rejected leaves no output shard on disk and clears OutputPaths, on
// both the bucketed and fast paths (so sfu -o/-od and sfl -od all stay tidy).
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
