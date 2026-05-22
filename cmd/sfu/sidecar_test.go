package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// basic round-trip via streamSidecarKeys. order isnt contract, sort both
func TestSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_test.txt.zst")
	wantA := []uint64{1, 2, 3, 4, 5}
	wantB := []uint64{100, 200, 300, 400}
	want := append(append([]uint64{}, wantA...), wantB...)

	count := writeSidecarKeysForTest(t, archive, want)
	if wantCount := uint64(len(want)); count != wantCount {
		t.Fatalf("count = %d, want %d", count, wantCount)
	}

	hdr, err := readSidecarHeader(sidecarPathForArchive(archive))
	if err != nil {
		t.Fatalf("readHeader: %v", err)
	}
	if hdr.keyCount != count {
		t.Errorf("header keyCount = %d, want %d", hdr.keyCount, count)
	}
	if hdr.formatVersion != sidecarFormatVer || hdr.hashAlgo != sidecarHashAlgoXX {
		t.Errorf("header version mismatch: %+v", hdr)
	}
	if hdr.parserVersion != parserVersion {
		t.Errorf("header parserVersion = %d, want %d", hdr.parserVersion, parserVersion)
	}

	var got []uint64
	if err := streamSidecarKeys(sidecarPathForArchive(archive), func(k uint64) error {
		got = append(got, k)
		return nil
	}); err != nil {
		t.Fatalf("streamKeys: %v", err)
	}

	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("key[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestStreamSidecarKeyBytesBatchesReads(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_batched.txt.zst")
	want := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	writeSidecarKeysForTest(t, archive, want)

	var got []uint64
	var chunkLens []int
	if err := streamSidecarKeyBytes(sidecarPathForArchive(archive), 24, func(raw []byte) error {
		chunkLens = append(chunkLens, len(raw))
		if len(raw)%sidecarKeyBytes != 0 {
			t.Fatalf("chunk len %d is not key-aligned", len(raw))
		}
		for off := 0; off < len(raw); off += sidecarKeyBytes {
			got = append(got, binary.LittleEndian.Uint64(raw[off:off+sidecarKeyBytes]))
		}
		return nil
	}); err != nil {
		t.Fatalf("streamSidecarKeyBytes: %v", err)
	}

	if !sliceEqualUint64(got, want) {
		t.Fatalf("got keys %v, want %v", got, want)
	}
	wantLens := []int{24, 24, 24, 8}
	if len(chunkLens) != len(wantLens) {
		t.Fatalf("chunk lens = %v, want %v", chunkLens, wantLens)
	}
	for i := range wantLens {
		if chunkLens[i] != wantLens[i] {
			t.Fatalf("chunk lens = %v, want %v", chunkLens, wantLens)
		}
	}
}

// regen hot path writes .idx tmp directly while streaming archive,
// skips scratch-hashlog + copy. fix for "100% but still copying" stall
func TestDirectSidecarWriterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_direct.txt.zst")
	keys := []uint64{7, 11, 13, 17, 19}

	sw, err := newSidecarWriter(archive)
	if err != nil {
		t.Fatalf("newSidecarWriter: %v", err)
	}
	for _, k := range keys {
		if err := sw.WriteHash(k); err != nil {
			t.Fatalf("WriteHash: %v", err)
		}
	}
	count, err := sw.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if count != uint64(len(keys)) {
		t.Fatalf("count = %d, want %d", count, len(keys))
	}

	var got []uint64
	if err := streamSidecarKeys(sidecarPathForArchive(archive), func(k uint64) error {
		got = append(got, k)
		return nil
	}); err != nil {
		t.Fatalf("stream keys: %v", err)
	}
	if len(got) != len(keys) {
		t.Fatalf("got %d keys, want %d", len(got), len(keys))
	}
	for i := range keys {
		if got[i] != keys[i] {
			t.Errorf("key[%d] = %d, want %d", i, got[i], keys[i])
		}
	}

	matches, err := filepath.Glob(filepath.Join(dir, idxSubdirName, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("leftover direct sidecar tmp files: %v", matches)
	}
}

// atomic-write invariant: stream failure removes tmp, no malformed
// final .idx visible to next run
func TestDirectSidecarWriterAbortLeavesNoVisibleSidecar(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_abort.txt.zst")

	sw, err := newSidecarWriter(archive)
	if err != nil {
		t.Fatalf("newSidecarWriter: %v", err)
	}
	if err := sw.WriteHash(42); err != nil {
		t.Fatalf("WriteHash: %v", err)
	}
	if err := sw.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := readSidecarHeader(sidecarPathForArchive(archive)); !errors.Is(err, errSidecarMissing) {
		t.Fatalf("final sidecar err = %v, want errSidecarMissing", err)
	}
}

// first -od run, 0 lines = header-only sidecar, must still be valid
func TestSidecarEmptyRun(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_test.txt.zst")
	count := writeSidecarKeysForTest(t, archive, nil)
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
	hdr, err := readSidecarHeader(sidecarPathForArchive(archive))
	if err != nil {
		t.Fatalf("read empty: %v", err)
	}
	if hdr.keyCount != 0 {
		t.Errorf("empty sidecar keyCount = %d", hdr.keyCount)
	}
	calls := 0
	if err := streamSidecarKeys(sidecarPathForArchive(archive), func(k uint64) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("stream empty: %v", err)
	}
	if calls != 0 {
		t.Errorf("expected 0 keys from empty sidecar, got %d", calls)
	}
}

// missing sidecar = errSidecarMissing, od_scan uses this to trigger regen
func TestSidecarMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := readSidecarHeader(filepath.Join(dir, "nope.idx"))
	if !errors.Is(err, errSidecarMissing) {
		t.Errorf("err = %v, want errSidecarMissing", err)
	}
}

// bad magic = rejected. real cause: user-renamed junk file
func TestSidecarMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.idx")
	if err := os.WriteFile(p, []byte("NOPENOPENOPENOPENOPENOPENOPENOPE"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readSidecarHeader(p)
	if !errors.Is(err, errSidecarMalformed) {
		t.Errorf("err = %v, want errSidecarMalformed", err)
	}
}

// stale parser version = errSidecarStale, next run regens silently
func TestSidecarStale(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "stale.idx")
	hdr := makeSidecarHeader(0)
	// fake older binary by corrupting parserVersion bytes
	for i := 16; i < 24; i++ {
		hdr[i] = 0xff
	}
	if err := os.WriteFile(p, hdr[:], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readSidecarHeader(p)
	if !errors.Is(err, errSidecarStale) {
		t.Errorf("err = %v, want errSidecarStale", err)
	}
}

// header claims N keys, body has fewer = malformed. partially-flushed
// sidecar from a crash cant poison the next run
func TestSidecarBodyTruncated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trunc.idx")
	hdr := makeSidecarHeader(10) // claim 10, write 0
	if err := os.WriteFile(p, hdr[:], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readSidecarHeader(p)
	if !errors.Is(err, errSidecarMalformed) {
		t.Errorf("err = %v, want errSidecarMalformed", err)
	}
}

func TestSidecarRejectsOverflowingKeyCount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "overflow.idx")
	hdr := makeSidecarHeader(1 << 61) // keyCount*8 overflows int64
	if err := os.WriteFile(p, hdr[:], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readSidecarHeader(p)
	if !errors.Is(err, errSidecarMalformed) {
		t.Errorf("err = %v, want errSidecarMalformed", err)
	}
}

func TestSidecarWriterDoesNotFollowPreexistingTempSymlink(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_symlink.txt.zst")
	finalPath := sidecarPathForArchive(archive)
	if err := ensureIdxSubdir(dir); err != nil {
		t.Fatal(err)
	}
	tmpPath := fmt.Sprintf("%s.write.%d.tmp", finalPath, os.Getpid())
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, tmpPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	sw, err := newSidecarWriter(archive)
	if err != nil {
		t.Fatalf("newSidecarWriter: %v", err)
	}
	if sw.tmpPath == tmpPath {
		t.Fatalf("writer reused preexisting symlink tmp path %s", tmpPath)
	}
	if err := sw.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep me" {
		t.Fatalf("symlink target content = %q, want unchanged", got)
	}
}

// successful write must not leave .tmp around
func TestSidecarAtomicWriteCleanup(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_test.txt.zst")
	writeSidecarKeysForTest(t, archive, []uint64{0xdeadbeef})

	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("leftover .tmp files: %v", matches)
	}
}

// guard on-disk path convention vs refactors breaking old libraries
func TestSidecarPathForArchive(t *testing.T) {
	want := "/libs/sfu_dedup_idx/sfu_xxx.txt.zst.idx"
	if got := sidecarPathForArchive("/libs/sfu_xxx.txt.zst"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func writeSidecarKeysForTest(t *testing.T, archive string, keys []uint64) uint64 {
	t.Helper()
	sw, err := newSidecarWriter(archive)
	if err != nil {
		t.Fatalf("newSidecarWriter: %v", err)
	}
	for _, k := range keys {
		if err := sw.WriteHash(k); err != nil {
			t.Fatalf("WriteHash: %v", err)
		}
	}
	count, err := sw.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return count
}

func sliceEqualUint64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
