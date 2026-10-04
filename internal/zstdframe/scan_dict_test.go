package zstdframe_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"

	"github.com/klauspost/compress/zstd"
)

// writeDictZST encodes payload as a single zstd frame carrying a dictionary
// ID. Per RFC 8878 the FHD dictionary flag selects the ID field width:
// 1 byte for IDs < 256, 2 bytes for IDs < 1<<16, 4 bytes above.
func writeDictZST(t *testing.T, path string, dictID uint32, dict, payload []byte) {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderDictRaw(dictID, dict))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanFileDictionaryFrameIDs scans real dictionary-ID frames for all
// three ID widths and asserts the frame is delimited and measured correctly.
// Decoding requires the matching dictionary to be registered (zstd decoders
// reject unknown dictionary IDs outright).
func TestScanFileDictionaryFrameIDs(t *testing.T) {
	dict := bytes.Repeat([]byte("dictionary-content-"), 32)
	payload := []byte("alpha line\nbeta needle line\ngamma\n")
	for _, tc := range []struct {
		name   string
		dictID uint32
	}{
		{"1-byte-id", 0x42},
		{"2-byte-id", 0x1234},
		{"4-byte-id", 0x00010001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zstdframe.RegisterDecoderDict(tc.dictID, dict)
			dir := t.TempDir()
			path := filepath.Join(dir, "dict.zst")
			writeDictZST(t, path, tc.dictID, dict, payload)

			frames, err := zstdframe.ScanFile(context.Background(), path, nil, nil)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(frames) != 1 {
				t.Fatalf("frames = %d, want 1", len(frames))
			}
			if frames[0].UncompressedStart != 0 || frames[0].UncompressedEnd != int64(len(payload)) {
				t.Fatalf("uncompressed range = [%d,%d), want [0,%d)",
					frames[0].UncompressedStart, frames[0].UncompressedEnd, len(payload))
			}
		})
	}
}
