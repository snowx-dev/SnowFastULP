package zstdframe_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
)

// craftedFrame builds a minimal valid zstd frame: magic + FHD 0x00 (no
// single-segment, no checksum, no dict ID) + the given window descriptor +
// one raw (uncompressed) block marked last, containing payload.
func craftedFrame(windowDesc byte, payload []byte) []byte {
	f := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, windowDesc}
	// block header: last=1 (bit 0), type=raw (bits 1-2 = 00), size (bits 3+)
	hdr := uint32(len(payload))<<3 | 1
	f = append(f, byte(hdr), byte(hdr>>8), byte(hdr>>16))
	return append(f, payload...)
}

// window descriptor 0x90: exponent = 0x90>>3 = 18 → windowLog 28 → 256 MiB
const oversizedWindowDesc byte = 0x90

// TestScanFileLongWindowErrorActionable: a frame declaring a window above the
// 128 MiB decoder cap must fail with a user-facing error that names the cap
// and the remediation (re-compress without zstd --long, or window ≤128 MiB)
// — not the bare "window size exceeded".
func TestScanFileLongWindowErrorActionable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "longwin.zst")
	if err := os.WriteFile(path, craftedFrame(oversizedWindowDesc, []byte("hello")), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := zstdframe.ScanFile(context.Background(), path, nil, nil)
	if err == nil {
		t.Fatal("frame declaring a 256 MiB window must fail the 128 MiB cap")
	}
	if !errors.Is(err, zstd.ErrWindowSizeExceeded) {
		t.Fatalf("err = %v, want it to wrap zstd.ErrWindowSizeExceeded", err)
	}
	msg := err.Error()
	for _, want := range []string{"128 MiB", "--long"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q:\n%s", want, msg)
		}
	}
}

// TestScanFileNormalWindowFramePasses: the same crafted frame shape with a
// small window descriptor decodes fine — the cap only rejects oversized
// window declarations.
func TestScanFileNormalWindowFramePasses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "normal.zst")
	payload := []byte("hello")
	if err := os.WriteFile(path, craftedFrame(0x00, payload), 0o644); err != nil {
		t.Fatal(err)
	}

	frames, err := zstdframe.ScanFile(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("normal frame must pass: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].UncompressedEnd != int64(len(payload)) {
		t.Fatalf("uncompressed end = %d, want %d", frames[0].UncompressedEnd, len(payload))
	}
}
