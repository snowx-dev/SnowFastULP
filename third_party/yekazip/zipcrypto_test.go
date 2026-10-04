package zip

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func assertBoundedAlloc(t *testing.T, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 1<<20 {
		t.Fatalf("allocation delta = %d bytes, want bounded (< 1 MiB): decryptor must not scale with the declared size", delta)
	}
}

// A declared compressed size far larger than the actual bytes behind the
// SectionReader must decrypt without allocating proportionally: memory is
// O(read buffer), not O(declared size), and a truncated payload surfaces as
// ErrUnexpectedEOF instead of a clean EOF.
func TestZipCryptoDecryptorBoundedAllocation(t *testing.T) {
	z := NewZipCrypto([]byte("pw"))
	enc := z.Encrypt([]byte("0123456789abcdef")) // 12-byte header + 4-byte body
	rr := io.NewSectionReader(bytes.NewReader(enc), 0, 1<<26)

	var rc io.Reader
	assertBoundedAlloc(t, func() {
		r, err := ZipCryptoDecryptor(rr, []byte("pw"))
		if err != nil {
			t.Fatalf("ZipCryptoDecryptor: %v", err)
		}
		rc = r
	})

	buf := make([]byte, 32)
	n, err := rc.Read(buf)
	if n != 4 || string(buf[:n]) != "cdef" {
		t.Fatalf("first read = (%d, %q, %v), want (4, %q, ...)", n, buf[:n], err, "cdef")
	}
	// The declared size promises 64 MiB of payload but the stream ended after
	// 4 bytes: the short read must surface, not masquerade as a clean EOF.
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("first read err = %v, want io.ErrUnexpectedEOF (declared size exceeds available bytes)", err)
	}
	_, err = rc.Read(buf)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("second read err = %v, want sticky io.ErrUnexpectedEOF", err)
	}
}

// A declared compressed size below the 12-byte encryption header is malformed.
func TestZipCryptoDecryptorRejectsShortDeclaredSize(t *testing.T) {
	enc := NewZipCrypto([]byte("pw")).Encrypt([]byte("short"))
	for _, size := range []int64{0, 5, 11} {
		rr := io.NewSectionReader(bytes.NewReader(enc), 0, size)
		if _, err := ZipCryptoDecryptor(rr, []byte("pw")); !errors.Is(err, ErrFormat) {
			t.Fatalf("declared size %d: err = %v, want ErrFormat", size, err)
		}
	}
}

// End-to-end: a member whose central directory declares a 1 GiB compressed
// size over a few real bytes must Open (no proportional allocation) and fail
// on first read with a bounded error.
func TestOpenHugeDeclaredCompressedSizeBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.zip")
	if err := writeZipCryptoEntry(path, "secret", "a.txt", "body-bytes", false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const huge = int64(1) << 30
	// Patch the compressed size in both the local header (offset 18) and the
	// central-directory header (offset 20): both carry uint32(len(enc)).
	want := uint32(zipCryptoHeaderLen + len("body-bytes"))
	pat := []byte{byte(want), byte(want >> 8), byte(want >> 16), byte(want >> 24)}
	patchSize := func(off int) {
		v := uint32(huge)
		raw[off] = byte(v)
		raw[off+1] = byte(v >> 8)
		raw[off+2] = byte(v >> 16)
		raw[off+3] = byte(v >> 24)
	}
	found := 0
	for i := 0; i+4 <= len(raw); i++ {
		if bytes.Equal(raw[i:i+4], pat) {
			patchSize(i)
			found++
			i += 3
		}
	}
	if found != 2 {
		t.Fatalf("patched %d size fields, want 2 (local + central)", found)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	file := firstFile(t, path)
	file.SetPassword("secret")

	var rc io.ReadCloser
	assertBoundedAlloc(t, func() {
		r, err := file.Open()
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		rc = r
	})
	defer rc.Close()
	var rerr error
	assertBoundedAlloc(t, func() {
		_, rerr = io.ReadAll(rc)
	})
	if rerr == nil || errors.Is(rerr, io.EOF) {
		t.Fatalf("read err = %v, want a bounded truncation error", rerr)
	}
}
