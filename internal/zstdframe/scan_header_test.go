package zstdframe

import (
	"bytes"
	"testing"
)

// TestZstdFrameHeaderLenDictionaryFlags pins the FHD dictionary-flag field
// widths from RFC 8878 §3.1.1.1: flag 1 → 1-byte ID, flag 2 → 2-byte ID,
// flag 3 → 4-byte ID. The pre-fix code treated flags 2 and 3 as 4-byte,
// misdelimiting every 2-byte-dictionary frame. Headers here use
// singleSegment=1 with a 1-byte FCS, so headerLen = 1 (FHD) + idBytes + 1.
func TestZstdFrameHeaderLenDictionaryFlags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dictFlag byte
		idBytes  int
		want     int
	}{
		{"no-dict", 0, 0, 2},
		{"1-byte-id", 1, 1, 3},
		{"2-byte-id", 2, 2, 4},
		{"4-byte-id", 3, 4, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// magic + FHD(dictFlag|singleSegment, 1-byte FCS) + ID + FCS
			raw := []byte{0x28, 0xb5, 0x2f, 0xfd}
			raw = append(raw, tc.dictFlag|0x08) // singleSegment=1, FCS flag=0
			for i := 0; i < tc.idBytes; i++ {
				raw = append(raw, byte(0x10+i))
			}
			raw = append(raw, 0x2b) // FCS field

			got, _, err := zstdFrameHeaderLen(bytes.NewReader(raw), 4, int64(len(raw)))
			if err != nil {
				t.Fatalf("header len: %v", err)
			}
			if got != tc.want {
				t.Fatalf("dictFlag=%d: headerLen = %d, want %d", tc.dictFlag, got, tc.want)
			}
		})
	}
}
