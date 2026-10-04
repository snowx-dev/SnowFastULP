package zip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// buildCentralDirHeader serializes a minimal central-directory header with the
// given extra field, mirroring the byte layout readDirectoryHeader parses.
func buildCentralDirHeader(t *testing.T, extra []byte) []byte {
	t.Helper()
	name := []byte("a.txt")
	var b bytes.Buffer
	w := func(v any) {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	w(uint32(directoryHeaderSignature))
	w(uint16(20))         // creator version
	w(uint16(20))         // reader version
	w(uint16(0))          // flags
	w(uint16(0))          // method (store)
	w(uint16(0))          // mod time
	w(uint16(0))          // mod date
	w(uint32(0))          // crc32
	w(uint32(0))          // compressed size
	w(uint32(0))          // uncompressed size
	w(uint16(len(name)))  // filename len
	w(uint16(len(extra))) // extra len
	w(uint16(0))          // comment len
	w(uint16(0))          // disk number start
	w(uint16(0))          // internal attrs
	w(uint32(0))          // external attrs
	w(uint32(0))          // local header offset
	b.Write(name)
	b.Write(extra)
	return b.Bytes()
}

// buildAESExtra builds a WinZip AES extra field: tag 0x9901, declared payload
// size, then payload bytes. Payloads < 7 are malformed.
func buildAESExtra(t *testing.T, payload []byte) []byte {
	t.Helper()
	extra := make([]byte, 0, 4+len(payload))
	extra = binary.LittleEndian.AppendUint16(extra, winzipAesExtraId)
	extra = binary.LittleEndian.AppendUint16(extra, uint16(len(payload)))
	extra = append(extra, payload...)
	return extra
}

// aesValidPayload is a well-formed 7-byte AES extra payload: version (AE-2),
// vendor "AE", strength AES-128, method store.
func aesValidPayload() []byte {
	return []byte{0x02, 0x00, 'A', 'E', 0x01, 0x00, 0x00}
}

// A central-directory WinZip AES extra (tag 0x9901) declaring fewer than 7
// payload bytes must be a normal ErrFormat, never a panic from reading past
// the buffer.
func TestReadDirectoryHeaderWinZipAESExtraBounds(t *testing.T) {
	cases := []struct {
		name     string
		payload  []byte
		wantErr  error
		wantAE   uint16
		wantStr  byte
		wantMeth uint16
	}{
		{"empty", nil, ErrFormat, 0, 0, 0},
		{"1-byte", []byte{0xA5}, ErrFormat, 0, 0, 0},
		{"2-byte", []byte{0xA5, 0xA5}, ErrFormat, 0, 0, 0},
		{"3-byte", []byte{0xA5, 0xA5, 0xA5}, ErrFormat, 0, 0, 0},
		{"4-byte", []byte{0xA5, 0xA5, 0xA5, 0xA5}, ErrFormat, 0, 0, 0},
		{"5-byte", []byte{0xA5, 0xA5, 0xA5, 0xA5, 0xA5}, ErrFormat, 0, 0, 0},
		{"6-byte", []byte{0xA5, 0xA5, 0xA5, 0xA5, 0xA5, 0xA5}, ErrFormat, 0, 0, 0},
		{"7-byte", aesValidPayload(), nil, 2, 1, 0},
		// Trailing zero bytes are legal padding.
		{"8-byte-padded", append(append([]byte{}, aesValidPayload()...), 0x00), nil, 2, 1, 0},
		{"9-byte-padded", append(append([]byte{}, aesValidPayload()...), 0x00, 0x00), nil, 2, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hdr := buildCentralDirHeader(t, buildAESExtra(t, tc.payload))
			f := &File{}
			err := readDirectoryHeader(f, bytes.NewReader(hdr))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("readDirectoryHeader err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				return
			}
			if f.ae != tc.wantAE || f.aesStrength != tc.wantStr || f.Method != tc.wantMeth {
				t.Fatalf("parsed ae=%d strength=%d method=%d, want %d/%d/%d",
					f.ae, f.aesStrength, f.Method, tc.wantAE, tc.wantStr, tc.wantMeth)
			}
		})
	}
}
