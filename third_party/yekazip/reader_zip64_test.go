package zip

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Regression for the ZIP64 extra-field parse: a member with small (non-sentinel)
// sizes but a >4GB local-header offset stores ONLY the 8-byte offset in the
// ZIP64 extra. The upstream code read that offset into UncompressedSize64 and
// left headerOffset at the 0xFFFFFFFF sentinel. The fix consumes each ZIP64
// field only when its base value is the sentinel.
func TestReadDirectoryHeaderZip64OffsetOnly(t *testing.T) {
	const wantOffset = 0x1_2345_6789 // > 4GB, so the base field must be sentinel

	name := []byte("a.txt")
	// ZIP64 extra: tag 0x0001, size 8, then the 8-byte real offset.
	extra := make([]byte, 0, 12)
	extra = binary.LittleEndian.AppendUint16(extra, zip64ExtraId)
	extra = binary.LittleEndian.AppendUint16(extra, 8)
	extra = binary.LittleEndian.AppendUint64(extra, uint64(wantOffset))

	var b bytes.Buffer
	w := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	w(uint32(directoryHeaderSignature))
	w(uint16(20))                   // creator version
	w(uint16(20))                   // reader version
	w(uint16(0))                    // flags
	w(uint16(0))                    // method (store)
	w(uint16(0))                    // mod time
	w(uint16(0))                    // mod date
	w(uint32(0))                    // crc32
	w(uint32(2))                    // compressed size (NOT sentinel)
	w(uint32(2))                    // uncompressed size (NOT sentinel)
	w(uint16(len(name)))            // filename len
	w(uint16(len(extra)))           // extra len
	w(uint16(0))                    // comment len
	w(uint16(0))                    // disk number start
	w(uint16(0))                    // internal attrs
	w(uint32(0))                    // external attrs
	w(uint32(0xFFFFFFFF))           // local header offset = sentinel
	b.Write(name)
	b.Write(extra)

	f := &File{}
	if err := readDirectoryHeader(f, &b); err != nil {
		t.Fatalf("readDirectoryHeader: %v", err)
	}
	if f.headerOffset != wantOffset {
		t.Errorf("headerOffset = %#x, want %#x (offset misparsed)", f.headerOffset, wantOffset)
	}
	if f.UncompressedSize64 != 2 {
		t.Errorf("UncompressedSize64 = %d, want 2 (offset leaked into size field)", f.UncompressedSize64)
	}
	if f.CompressedSize64 != 2 {
		t.Errorf("CompressedSize64 = %d, want 2", f.CompressedSize64)
	}
}
