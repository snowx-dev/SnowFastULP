package ulpengine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func TestSFUODWritesSearchSidecarForSFS(t *testing.T) {
	libDir := t.TempDir()
	in := filepath.Join(t.TempDir(), "in.txt")
	writeFileContent(t, in, "https://example.com:user@example.com:needle\n")

	stamp := "sfs_integ"
	r, err := Resolve(Config{
		Inputs:       []string{in},
		Output:       filepath.Join(libDir, "sfu_"+stamp+".txt.zst"),
		TempDir:      filepath.Join(libDir, ".stage"),
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     stamp,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := Run(context.Background(), &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      filepath.Join(libDir, ".stage"),
	}, &Metrics{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	arch := filepath.Join(libDir, "sfu_"+stamp+".txt.zst")
	sidecar := searchidx.LibrarySidecarPath(arch)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("search sidecar missing at %s: %v", sidecar, err)
	}
	sc, err := index.Load(sidecar)
	if err != nil {
		t.Fatalf("index.Load: %v", err)
	}
	if sc.Format != searchidx.FormatName || len(sc.Chunks) == 0 {
		t.Fatalf("unexpected sidecar: format=%q chunks=%d", sc.Format, len(sc.Chunks))
	}
}

func TestSFSExecFindsHitAfterSFUOD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sfs exec integration in -short mode")
	}

	root := repoRoot(t)
	sfsBin := filepath.Join(t.TempDir(), "sfs")
	cmd := exec.Command("go", "build", "-o", sfsBin, "./cmd/sfs")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfs: %v\n%s", err, out)
	}

	libDir := t.TempDir()
	in := filepath.Join(t.TempDir(), "in.txt")
	writeFileContent(t, in, "https://example.com:user@example.com:needle\n")

	stamp := "sfs_exec"
	r, err := Resolve(Config{
		Inputs:       []string{in},
		Output:       filepath.Join(libDir, "sfu_"+stamp+".txt.zst"),
		TempDir:      filepath.Join(libDir, ".stage"),
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     stamp,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := Run(context.Background(), &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      filepath.Join(libDir, ".stage"),
	}, &Metrics{}); err != nil {
		t.Fatalf("run: %v", err)
	}

	hitsPath := filepath.Join(libDir, "hits.txt")
	hitsPath, err = filepath.Abs(hitsPath)
	if err != nil {
		t.Fatal(err)
	}
	sfsCmd := exec.Command(sfsBin, libDir, "needle", "-o", hitsPath, "-silent")
	if out, err := sfsCmd.CombinedOutput(); err != nil {
		t.Fatalf("sfs: %v\n%s", err, out)
	}
	data, err := os.ReadFile(hitsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "needle") {
		t.Fatalf("hits file %q: %q", hitsPath, data)
	}
}
