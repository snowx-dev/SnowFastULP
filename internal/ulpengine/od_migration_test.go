package ulpengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

	runSFU := func(input, stamp string, m *Metrics) {
		t.Helper()
		rc, err := Resolve(Config{
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
		if err := Run(context.Background(), &Resolved{
			Cfg:          rc.Cfg,
			TotalInputs:  rc.TotalInputs,
			mem:          rc.mem,
			BucketCount:  4,
			Workers:      1,
			DedupWorkers: 1,
			chunkBytes:   1 << 20,
			TempDir:      filepath.Join(libDir, ".stage_"+stamp),
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
	runSFU(in1, "one", &Metrics{})

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
	m2 := &Metrics{}
	runSFU(in2, "two", m2)

	if got := m2.LinesSkippedByDest.Load(); got != 2 {
		t.Errorf("linesSkippedByDest = %d, want 2 (library dups via upgraded sidecar)", got)
	}
	if got := m2.LinesUnique.Load(); got != 1 {
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

// after migration, a second -od run must treat sidecars as fresh v3 (no re-upgrade).
func TestODMigratesIdempotentSecondRun(t *testing.T) {
	libDir := t.TempDir()
	archive := filepath.Join(libDir, "sfu_one.txt.zst")
	sidecar := sidecarPathForArchive(archive)

	// seed v2 sidecar + dummy archive (mtime older than sidecar)
	if err := os.WriteFile(archive, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeV2Sidecar(t, archive, []uint64{1, 5, 3, 5, 9})
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(archive, past, past); err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	res1, err := runODScanSync(context.Background(), odConfig{
		Dest:            libDir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if res1.ArchivesFresh != 1 {
		t.Fatalf("first scan fresh = %d, want 1", res1.ArchivesFresh)
	}
	hdr, err := readSidecarHeader(sidecar)
	if err != nil || !hdr.sorted() {
		t.Fatalf("after first scan sidecar should be v3: err=%v", err)
	}

	res2, err := runODScanSync(context.Background(), odConfig{
		Dest:            libDir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if res2.ArchivesFresh != 1 || res2.ArchivesRegen != 0 {
		t.Errorf("second scan = fresh %d regen %d, want 1/0 (idempotent)", res2.ArchivesFresh, res2.ArchivesRegen)
	}
}

// migration must not touch archive bytes or mtimes on disk.
func TestODMigratesArchiveMtimeUnchanged(t *testing.T) {
	libDir := t.TempDir()
	archive := filepath.Join(libDir, "sfu_one.txt.zst")
	payload := []byte("compressed archive payload bytes")
	if err := os.WriteFile(archive, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	writeV2Sidecar(t, archive, []uint64{42, 7, 42})
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(archive, past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	if _, err := runODScanSync(context.Background(), odConfig{
		Dest:            libDir,
		CurrentRunStamp: "sfu_self",
		Buckets:         4,
		TempDir:         tempDir,
	}, &ODMetrics{}); err != nil {
		t.Fatalf("runODScan: %v", err)
	}

	after, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("archive mtime changed: before=%v after=%v", before.ModTime(), after.ModTime())
	}
	got, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Error("archive payload was modified during index upgrade")
	}
}

// multi-part library: seed split archives, downgrade every part to v2, migrate
// in place, then dedup correctly across all parts.
func TestODMigratesMultiPartLibraryGolden(t *testing.T) {
	libDir := t.TempDir()
	stamp := "multipart"

	runSFU := func(input, RunStamp string, zstChunk int64, m *Metrics) []string {
		t.Helper()
		started := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
		r, err := Resolve(Config{
			Inputs:        []string{input},
			Output:        filepath.Join(libDir, "sfu_"+RunStamp+".txt.zst"),
			TempDir:       filepath.Join(libDir, ".stage_"+RunStamp),
			FastPathOff:   true,
			Buckets:       4,
			Compress:      true,
			ZstChunkLines: zstChunk,
			RunStarted:    started,
			RunStamp:      RunStamp,
			DestDedup:     true,
			DestDedupDir:  libDir,
		})
		if err != nil {
			t.Fatalf("resolve %s: %v", RunStamp, err)
		}
		r.TempDir = filepath.Join(libDir, ".stage_"+RunStamp)
		if err := Run(context.Background(), r, m); err != nil {
			t.Fatalf("run %s: %v", RunStamp, err)
		}
		return r.OutputPaths
	}

	// 12 unique lines → 3 parts at 4 lines/part
	var seedLines []string
	for i := 0; i < 12; i++ {
		seedLines = append(seedLines, fmt.Sprintf("https://host%d.example.com:user%d:pass%d", i, i, i))
	}
	in1 := filepath.Join(t.TempDir(), "seed.txt")
	writeFileContent(t, in1, strings.Join(seedLines, "\n")+"\n")

	paths := runSFU(in1, stamp, 4, &Metrics{})
	if len(paths) != 3 {
		t.Fatalf("seed run produced %d parts, want 3: %v", len(paths), paths)
	}

	archiveHashes := make(map[string]string, len(paths))
	past := time.Now().Add(-time.Hour)
	for _, p := range paths {
		sum, err := fileSHA256Hex(p)
		if err != nil {
			t.Fatal(err)
		}
		archiveHashes[p] = sum

		var keys []uint64
		sc := sidecarPathForArchive(p)
		if err := streamSidecarKeys(sc, func(k uint64) error { keys = append(keys, k); return nil }); err != nil {
			t.Fatalf("read v3 sidecar %s: %v", sc, err)
		}
		writeV2Sidecar(t, p, keys)
		if hdr, err := readSidecarHeader(sc); err != nil || hdr.sorted() {
			t.Fatalf("downgrade %s failed: err=%v", sc, err)
		}
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	// 6 dups + 2 new across parts
	var in2Lines []string
	in2Lines = append(in2Lines, seedLines[:6]...)
	in2Lines = append(in2Lines,
		"https://new1.example.com:u99:p99",
		"https://new2.example.com:u98:p98",
	)
	in2 := filepath.Join(t.TempDir(), "dedup.txt")
	writeFileContent(t, in2, strings.Join(in2Lines, "\n")+"\n")

	m2 := &Metrics{}
	runSFU(in2, stamp+"_run2", 4, m2)

	if got := m2.LinesSkippedByDest.Load(); got != 6 {
		t.Errorf("linesSkippedByDest = %d, want 6 (multi-part library dups)", got)
	}
	if got := m2.LinesUnique.Load(); got != 2 {
		t.Errorf("linesUnique = %d, want 2", got)
	}

	for _, p := range paths {
		if got, err := fileSHA256Hex(p); err != nil {
			t.Fatal(err)
		} else if got != archiveHashes[p] {
			t.Errorf("archive %s modified during migration", filepath.Base(p))
		}
		sc := sidecarPathForArchive(p)
		hdr, err := readSidecarHeader(sc)
		if err != nil {
			t.Fatalf("post-run sidecar %s: %v", sc, err)
		}
		if !hdr.sorted() || hdr.formatVersion != sidecarFormatV3 {
			t.Errorf("part sidecar not v3: %s formatVersion=%d", sc, hdr.formatVersion)
		}
		assertSortedUnique(t, readAllSidecarKeys(t, sc))
	}
}

func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
