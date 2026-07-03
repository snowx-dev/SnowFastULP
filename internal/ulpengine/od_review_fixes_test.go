package ulpengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// regenSidecarForPart is a test convenience: single-part regen without the
// worker pool. Always parseLoose because past archives may include loose-only
// shapes (eg host:port:user:pw, no TLD); loose tries strict first so the cost
// is ~zero on strict-parseable lines.
func regenSidecarForPart(ctx context.Context, part archivePart, decoderConcurrency int, ws *WorkerStatus, m *ODMetrics) (uint64, error) {
	if ws != nil {
		defer ws.ArchivePath.Store(nil)
	}
	fmtr := newLineFormatter()
	return processPartTask(ctx, partTask{part: part}, decoderConcurrency, ws, fmtr, m)
}

// writes lines into a fresh sfu_<stamp>.txt.zst, mirrors prod output
func helperWriteArchive(t *testing.T, dir, stamp string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, "sfu_"+stamp+".txt.zst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range lines {
		if _, err := enc.Write([]byte(ln + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// regen always uses loose mode regardless of current -loose setting.
// pre-fix, strict regen vs loose-built archive silently dropped lines
// from the sidecar = false negatives in next run's dedup
func TestRegenForcesLooseParsing(t *testing.T) {
	dir := t.TempDir()
	// host:port:user:pw rejected by strict, accepted by loose
	looseOnlyLine := "203.0.113.5:8080:admin:p@ss"
	archive := helperWriteArchive(t, dir, "20260514_loose", []string{looseOnlyLine})

	part := archivePart{path: archive, partNum: 0, sidecarPath: sidecarPathForArchive(archive)}
	count, err := regenSidecarForPart(context.Background(), part, 1, nil, nil)
	if err != nil {
		t.Fatalf("regen: %v", err)
	}
	if count != 1 {
		t.Errorf("regen forced-loose: got %d keys, want 1", count)
	}
	hdr, err := readSidecarHeader(part.sidecarPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if hdr.keyCount != 1 {
		t.Errorf("sidecar keyCount=%d, want 1", hdr.keyCount)
	}
}

// missing or non-dir -od must error, not silently no-op
func TestDiscoverArchiveRunsRejectsNonDir(t *testing.T) {
	t.Run("missing path", func(t *testing.T) {
		_, err := discoverArchiveRuns("/this/path/does/not/exist", "")
		if err == nil {
			t.Error("missing dir should return error, got nil")
		}
	})
	t.Run("regular file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "regular_file.txt")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := discoverArchiveRuns(f, "")
		if err == nil {
			t.Error("regular file should return error, got nil")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("expected 'not a directory' error, got: %v", err)
		}
	})
}

// corrupt zstd must surface as errCorruptArchive (skippable) not opaque fatal
func TestStreamArchiveLinesClassifiesCorruption(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "sfu_bad.txt.zst")
	// zstd magic but truncated mid-frame
	zstdMagic := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00}
	if err := os.WriteFile(bad, zstdMagic, 0o600); err != nil {
		t.Fatal(err)
	}
	err := streamArchiveLines(context.Background(), bad, 1, nil, func(string) error { return nil }, nil)
	if err == nil {
		t.Fatal("expected error from truncated zstd, got nil")
	}
	if !errors.Is(err, errCorruptArchive) {
		t.Errorf("expected errCorruptArchive sentinel, got: %v (type %T)", err, err)
	}
}

// low-RAM + huge library, chooser should pick large B (perBucket drops)
func TestChooseBucketCountODAuxFloorOnSparseRAM(t *testing.T) {
	// 10e9 keys * 8 = 80 GB; at the 128 MiB dest-set budget the aux floor is
	// 640 -> 1024 after pow2, still dominating the input-side estimate.
	const libBytes = 10_000_000_000 * 8
	b := chooseBucketCount(int64(100<<30), int64(libBytes),
		memInfo{total: 8 << 30, available: 2 << 30}, 4, minBuckets, maxBuckets)
	if b < 1024 {
		t.Errorf("low-RAM + huge library: B=%d, want ≥ 1024", b)
	}
	if b > maxBuckets {
		t.Errorf("B=%d exceeds maxBuckets=%d", b, maxBuckets)
	}
}

// sidecar writer must use pid-tagged tmp, not trample another pid's stale tmp
func TestSidecarWriterIgnoresOtherPidTmp(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_test.txt.zst")
	if err := os.WriteFile(archive, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// stale tmp w/ another pid, must remain untouched
	if err := ensureIdxSubdir(filepath.Dir(archive)); err != nil {
		t.Fatal(err)
	}
	otherPidTmp := fmt.Sprintf("%s.write.99999.tmp", sidecarPathForArchive(archive))
	if err := os.WriteFile(otherPidTmp, bytes.Repeat([]byte{0xff}, 64), 0o600); err != nil {
		t.Fatal(err)
	}

	count := writeSidecarKeysForTest(t, archive, []uint64{0xCAFEBABE})
	if count != 1 {
		t.Errorf("count=%d, want 1", count)
	}
	if _, err := os.Stat(otherPidTmp); err != nil {
		t.Errorf("writer touched another pid's tmp: %v", err)
	}
	hdr, err := readSidecarHeader(sidecarPathForArchive(archive))
	if err != nil {
		t.Fatalf("read final sidecar: %v", err)
	}
	if hdr.keyCount != 1 {
		t.Errorf("sidecar keyCount=%d, want 1", hdr.keyCount)
	}
}

// >50% unreadable archives must fail fast, not silently weaken dedup
func TestRunODScanRefusesMajorityCorrupt(t *testing.T) {
	dir := t.TempDir()
	// 2 corrupt + 1 valid
	corruptMagic := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00}
	for _, stamp := range []string{"20260514_bad1", "20260514_bad2"} {
		p := filepath.Join(dir, "sfu_"+stamp+".txt.zst")
		if err := os.WriteFile(p, corruptMagic, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	helperWriteArchive(t, dir, "20260514_good", []string{"example.com:alice:pw1"})

	odm := &ODMetrics{}
	_, err := runODScanSync(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "irrelevant_stamp_99999",
		Buckets:         64,
		TempDir:         t.TempDir(),
	}, odm)
	if err == nil {
		t.Fatal("expected error when >50% of archives are corrupt, got nil")
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("expected 'unreadable' in error, got: %v", err)
	}
}

// system errors (EACCES) must propagate fatal, not skip-with-warning.
// chmod 0000, EACCES isnt errCorruptArchive
func TestRunODScanFatalsOnSystemError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod 0000")
	}
	dir := t.TempDir()
	archive := helperWriteArchive(t, dir, "20260514_unreadable", []string{"example.com:bob:pw"})
	if err := os.Chmod(archive, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(archive, 0o600) // for TempDir cleanup

	odm := &ODMetrics{}
	_, err := runODScanSync(context.Background(), odConfig{
		Dest:            dir,
		CurrentRunStamp: "irrelevant_stamp_99999",
		Buckets:         64,
		TempDir:         t.TempDir(),
	}, odm)
	if err == nil {
		t.Fatal("expected fatal error for system-error archive, got nil")
	}
}

// phase-0 TUI gets its own distinct enum value
func TestPhasePhase0IsDistinctFromShard(t *testing.T) {
	if PhasePhase0 == PhaseShard {
		t.Error("phasePhase0 must be distinct from phaseShard")
	}
	if PhasePhase0 == PhaseInit {
		t.Error("phasePhase0 must be distinct from phaseInit")
	}
	if PhasePhase0 == PhaseDedup {
		t.Error("phasePhase0 must be distinct from phaseDedup")
	}
}

// regenParts must surface decoder-per-archive concurrency in its event
// line, and the value must clear floor(GOMAXPROCS/min(GOMAXPROCS,N)).
// end-to-end check, not a self-referential math tautology
func TestAdaptiveDecoderConcurrencyEventLine(t *testing.T) {
	dir := t.TempDir()
	parts := buildParts(t, dir, "sfu_dec_concurrency", 1, 50)

	logPath := filepath.Join(t.TempDir(), "sfu.log")
	dbg, err := NewDebugLog(logPath)
	if err != nil {
		t.Fatalf("newDebugLog: %v", err)
	}

	m := &ODMetrics{}
	if _, err := regenParts(context.Background(), parts, odConfig{Debug: dbg}, m); err != nil {
		t.Fatalf("regenParts: %v", err)
	}
	if err := dbg.Close(); err != nil {
		t.Fatalf("dbg.Close: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(body)

	// event format:
	//   [event +0s] [od] regen pool: workers=W decoder_concurrency_per_archive=N (GOMAXPROCS=X, parts=P)
	const marker = "regen pool: workers="
	idx := strings.Index(logText, marker)
	if idx < 0 {
		t.Fatalf("debug log missing 'regen pool' event line:\n%s", logText)
	}
	rest := logText[idx+len(marker):]
	var workers, decConc, gomp, parsedParts int
	if _, err := fmt.Sscanf(rest, "%d decoder_concurrency_per_archive=%d (GOMAXPROCS=%d, parts=%d)",
		&workers, &decConc, &gomp, &parsedParts); err != nil {
		t.Fatalf("parse event line: %v\nrest=%q", err, rest)
	}

	if parsedParts != 1 {
		t.Errorf("event parts=%d, want 1", parsedParts)
	}
	// 1 task + GOMAXPROCS cores, decoder fan-out should use the headroom.
	// workers=1 (cant split a single task), decConc >= GOMAXPROCS/workers
	if workers != 1 {
		t.Errorf("single-archive workers = %d, want 1", workers)
	}
	wantMin := gomp / max1(workers)
	if decConc < wantMin {
		t.Errorf("decoder_concurrency_per_archive=%d below floor %d (GOMAXPROCS=%d, workers=%d)",
			decConc, wantMin, gomp, workers)
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// plain-text path ignores decoderConcurrency, any value should pass
func TestStreamArchiveLinesAcceptsConcurrency(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(plain, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dc := range []int{-1, 0, 1, 4, 16} {
		var got int
		err := streamArchiveLines(context.Background(), plain, dc, nil, func(string) error {
			got++
			return nil
		}, nil)
		if err != nil {
			t.Errorf("concurrency=%d returned err=%v", dc, err)
		}
		if got != 3 {
			t.Errorf("concurrency=%d: lines=%d, want 3", dc, got)
		}
	}
}
