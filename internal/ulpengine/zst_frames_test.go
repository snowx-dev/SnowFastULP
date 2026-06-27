package ulpengine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

func countZstFrames(data []byte) int {
	const magic = 0xFD2FB528
	n := 0
	for i := 0; i+4 <= len(data); i++ {
		if binary.LittleEndian.Uint32(data[i:]) == magic {
			n++
		}
	}
	return n
}

func TestOutputSinkMultiFrameRotation(t *testing.T) {
	old := zstFrameUncompressedBytes
	zstFrameUncompressedBytes = 64
	t.Cleanup(func() { zstFrameUncompressedBytes = old })

	dir := t.TempDir()
	path := filepath.Join(dir, "multi.zst")
	sink, err := newOutputSink(path, true, false)
	if err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte("x"), 200)
	if err := sink.writeBatch(append(payload, '\n'), 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := sink.close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frames := countZstFrames(data)
	if frames < 2 {
		t.Fatalf("frames = %d, want >= 2", frames)
	}
}

func TestOutputSinkSearchSidecarOnlyWhenRequested(t *testing.T) {
	dir := t.TempDir()

	noIdxPath := filepath.Join(dir, "plain.zst")
	sink, err := newOutputSink(noIdxPath, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLine(sink, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if err := sink.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(searchSidecarPathForArchive(noIdxPath)); !os.IsNotExist(err) {
		t.Fatal("expected no search sidecar for writeSearchIdx=false")
	}

	withIdxPath := filepath.Join(dir, "library", "sfu_run_part1.txt.zst")
	if err := os.MkdirAll(filepath.Dir(withIdxPath), 0o755); err != nil {
		t.Fatal(err)
	}
	sink2, err := newOutputSink(withIdxPath, true, true)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("y"), 128)
	if err := sink2.writeBatch(append(line, '\n'), 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := sink2.close(); err != nil {
		t.Fatal(err)
	}
	sidecar := searchSidecarPathForArchive(withIdxPath)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("expected search sidecar at %s: %v", sidecar, err)
	}
}

func TestSearchSidecarPathForArchive(t *testing.T) {
	arch := "/lib/sfu_abc_part1.txt.zst"
	want := searchidx.LibrarySidecarPath(arch)
	if got := searchSidecarPathForArchive(arch); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
