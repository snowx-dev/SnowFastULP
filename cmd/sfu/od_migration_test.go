package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end migration: a library whose .idx is a legacy v2 (unsorted) sidecar
// must be transparently upgraded to sorted v3 on the next -od run and dedup
// correctly against it — exercising classify→upgradeSidecars→gather, which the
// upgradeSidecarToV3 unit test doesn't cover through the real pipeline.
func TestODMigratesLegacyV2SidecarThenDedups(t *testing.T) {
	libDir := t.TempDir()

	runSFU := func(input, stamp string, m *metrics) {
		t.Helper()
		rc, err := resolvePipelineConfig(pipelineConfig{
			Inputs:       []string{input},
			Output:       filepath.Join(libDir, "sfu_"+stamp+".txt.zst"),
			TempDir:      filepath.Join(libDir, ".stage_"+stamp),
			FastPathOff:  true,
			Buckets:      4,
			Compress:     true,
			DestDedup:    true,
			DestDedupDir: libDir,
			RunStamp:     stamp,
		})
		if err != nil {
			t.Fatalf("resolve %s: %v", stamp, err)
		}
		if err := run(context.Background(), &resolved{
			cfg:          rc.cfg,
			totalInputs:  rc.totalInputs,
			mem:          rc.mem,
			bucketCount:  4,
			workers:      1,
			dedupWorkers: 1,
			chunkBytes:   1 << 20,
			tempDir:      filepath.Join(libDir, ".stage_"+stamp),
		}, m); err != nil {
			t.Fatalf("run %s: %v", stamp, err)
		}
	}

	// seed the library (run 1 writes archive + a sorted v3 sidecar)
	in1 := filepath.Join(t.TempDir(), "in1.txt")
	writeFileContent(t, in1, strings.Join([]string{
		"https://a.example.com:u1:p1",
		"https://b.example.com:u2:p2",
		"https://c.example.com:u3:p3",
	}, "\n")+"\n")
	runSFU(in1, "one", &metrics{})

	archive := filepath.Join(libDir, "sfu_one.txt.zst")
	sidecar := sidecarPathForArchive(archive)

	// downgrade the sidecar to a legacy v2 (unsorted) using its own keys, so the
	// next run sees a fresh-but-v2 sidecar and must upgrade it in place.
	var keys []uint64
	if err := streamSidecarKeys(sidecar, func(k uint64) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatalf("read v3 sidecar: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("seed sidecar key count = %d, want 3", len(keys))
	}
	writeV2Sidecar(t, archive, keys)
	if hdr, err := readSidecarHeader(sidecar); err != nil || hdr.sorted() {
		t.Fatalf("downgrade failed: should be readable v2; err=%v", err)
	}
	// keep the archive older than the (just-rewritten) sidecar → classify "fresh"
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(archive, past, past); err != nil {
		t.Fatal(err)
	}

	// run 2: 2 dups with the library + 1 new. the v2 sidecar must be upgraded
	// and used to skip the dups.
	in2 := filepath.Join(t.TempDir(), "in2.txt")
	writeFileContent(t, in2, strings.Join([]string{
		"https://a.example.com:u1:p1", // dup
		"https://b.example.com:u2:p2", // dup
		"https://z.example.com:u9:p9", // new
	}, "\n")+"\n")
	m2 := &metrics{}
	runSFU(in2, "two", m2)

	if got := m2.linesSkippedByDest.Load(); got != 2 {
		t.Errorf("linesSkippedByDest = %d, want 2 (library dups via upgraded sidecar)", got)
	}
	if got := m2.linesUnique.Load(); got != 1 {
		t.Errorf("linesUnique = %d, want 1", got)
	}

	// the library sidecar must now be sorted v3 (upgraded in place, archive untouched)
	hdr, err := readSidecarHeader(sidecar)
	if err != nil {
		t.Fatalf("post-run sidecar: %v", err)
	}
	if !hdr.sorted() || hdr.formatVersion != sidecarFormatV3 {
		t.Errorf("sidecar not upgraded: formatVersion=%d", hdr.formatVersion)
	}
}
