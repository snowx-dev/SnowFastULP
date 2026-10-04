package zip

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeEncryptedEntry writes one AES-encrypted member using the writer API.
func writeEncryptedEntry(t *testing.T, path, password, name, body string, enc EncryptionMethod) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := NewWriter(f)
	fh := &FileHeader{Name: name}
	fh.SetModTime(time.Now())
	fh.SetPassword(password)
	fh.SetEncryptionMethod(enc)
	w, err := zw.CreateHeader(fh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func firstFile(t *testing.T, path string) *File {
	t.Helper()
	zr, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { zr.Close() })
	if len(zr.File) != 1 {
		t.Fatalf("archive has %d files, want 1", len(zr.File))
	}
	return zr.File[0]
}

func TestVerifyPasswordUnencrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := NewWriter(f)
	w, err := zw.Create("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstFile(t, path).VerifyPassword(); err != nil {
		t.Fatalf("unencrypted VerifyPassword = %v, want nil", err)
	}
}

func TestVerifyPasswordWinZipAES(t *testing.T) {
	dir := t.TempDir()
	body := "URL: https://example.invalid/login"
	for _, strength := range []EncryptionMethod{AES128Encryption, AES192Encryption, AES256Encryption} {
		path := filepath.Join(dir, "aes.zip")
		writeEncryptedEntry(t, path, "secret", "a.txt", body, strength)

		file := firstFile(t, path)
		if !file.IsEncrypted() {
			t.Fatal("entry is not flagged encrypted")
		}
		file = firstFile(t, path)
		file.SetPassword("secret")
		if err := file.VerifyPassword(); err != nil {
			t.Fatalf("AES strength %d: correct password = %v, want nil", strength, err)
		}
		// Reopen fresh so the wrong-password attempt cannot reuse state.
		file = firstFile(t, path)
		file.SetPassword("wrong")
		if err := file.VerifyPassword(); err != ErrPassword {
			t.Fatalf("AES strength %d: wrong password = %v, want ErrPassword", strength, err)
		}
		file = firstFile(t, path)
		file.password = nil
		if err := file.VerifyPassword(); err != ErrPassword {
			t.Fatalf("AES strength %d: no password = %v, want ErrPassword", strength, err)
		}
	}
}

func TestVerifyPasswordZipCrypto(t *testing.T) {
	dir := t.TempDir()
	body := "URL: https://example.invalid/login"

	// Spec-conformant ZipCrypto without a data descriptor: the check byte is
	// the high CRC byte.
	path := filepath.Join(dir, "zc-crc.zip")
	if err := writeZipCryptoEntry(path, "secret", "a.txt", body, false); err != nil {
		t.Fatal(err)
	}
	file := firstFile(t, path)
	if !file.IsEncrypted() || file.hasDataDescriptor() {
		t.Fatalf("fixture flags = %#x, want encrypted without bit 3", file.Flags)
	}
	file.SetPassword("secret")
	if err := file.VerifyPassword(); err != nil {
		t.Fatalf("correct password = %v, want nil", err)
	}
	file = firstFile(t, path)
	file.SetPassword("wrong")
	if err := file.VerifyPassword(); err != ErrPassword {
		t.Fatalf("wrong password = %v, want ErrPassword", err)
	}
	file = firstFile(t, path)
	file.password = nil
	if err := file.VerifyPassword(); err != ErrPassword {
		t.Fatalf("no password = %v, want ErrPassword", err)
	}

	// With general-purpose bit 3 (data descriptor) the check byte is the high
	// modified-time byte instead.
	path = filepath.Join(dir, "zc-dd.zip")
	if err := writeZipCryptoEntry(path, "secret", "a.txt", body, true); err != nil {
		t.Fatal(err)
	}
	file = firstFile(t, path)
	if !file.hasDataDescriptor() {
		t.Fatal("fixture must set bit 3")
	}
	file.SetPassword("secret")
	if err := file.VerifyPassword(); err != nil {
		t.Fatalf("correct password (descriptor) = %v, want nil", err)
	}
	file = firstFile(t, path)
	file.SetPassword("wrong")
	if err := file.VerifyPassword(); err != ErrPassword {
		t.Fatalf("wrong password (descriptor) = %v, want ErrPassword", err)
	}
}

// modTime is the DOS modification time stamped into the hand-built ZipCrypto
// fixtures; nonzero so the descriptor check byte discriminates passwords.
var modTime = uint16(0x1200)

// writeZipCryptoEntry hand-builds a spec-conformant ZipCrypto (traditional
// PKWARE) encrypted stored member: the last byte of the 12-byte encryption
// header carries crc>>24, or the high modified-time byte under a data
// descriptor (bit 3). The vendored writer's ZipCrypto path predates these
// check-byte semantics, so the fixture is built at the byte level.
func writeZipCryptoEntry(path, password, name, body string, descriptor bool) error {
	crc := crc32.ChecksumIEEE([]byte(body))
	var plain [12]byte
	for i := range plain[:10] {
		plain[i] = byte(0x11 * (i + 1)) // arbitrary, deterministic
	}
	if descriptor {
		plain[10], plain[11] = byte(modTime), byte(modTime>>8)
	} else {
		plain[10] = 0
		plain[11] = byte(crc >> 24)
	}
	z := NewZipCrypto([]byte(password))
	enc := make([]byte, 0, len(plain)+len(body))
	enc = append(enc, z.Encrypt(plain[:])...)
	enc = append(enc, z.Encrypt([]byte(body))...)

	flags := uint16(0x1)
	fileCRC := crc
	if descriptor {
		flags = 0x9
		fileCRC = 0 // CRC unknown up front, lives only in the descriptor
	}
	modDate := uint16(0x5921) // nonzero so the modified-time byte discriminates

	var b []byte
	b = binary.LittleEndian.AppendUint32(b, fileHeaderSignature)
	b = binary.LittleEndian.AppendUint16(b, zipVersion20)
	b = binary.LittleEndian.AppendUint16(b, flags)
	b = binary.LittleEndian.AppendUint16(b, 0) // store
	b = binary.LittleEndian.AppendUint16(b, modTime)
	b = binary.LittleEndian.AppendUint16(b, modDate)
	b = binary.LittleEndian.AppendUint32(b, fileCRC)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(enc)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(body)))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(name)))
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = append(b, name...)
	b = append(b, enc...)

	cdStart := len(b)
	b = binary.LittleEndian.AppendUint32(b, directoryHeaderSignature)
	b = binary.LittleEndian.AppendUint16(b, zipVersion20)
	b = binary.LittleEndian.AppendUint16(b, zipVersion20)
	b = binary.LittleEndian.AppendUint16(b, flags)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, modTime)
	b = binary.LittleEndian.AppendUint16(b, modDate)
	b = binary.LittleEndian.AppendUint32(b, fileCRC)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(enc)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(body)))
	b = binary.LittleEndian.AppendUint16(b, uint16(len(name)))
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 0) // local header at offset 0
	b = append(b, name...)
	cdLen := len(b) - cdStart

	b = binary.LittleEndian.AppendUint32(b, directoryEndSignature)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint16(b, 1)
	b = binary.LittleEndian.AppendUint32(b, uint32(cdLen))
	b = binary.LittleEndian.AppendUint32(b, uint32(cdStart))
	b = binary.LittleEndian.AppendUint16(b, 0)

	return os.WriteFile(path, b, 0o644)
}

// VerifyPassword must not consume or decompress the body: after a successful
// verification the full Open/Read round-trip still works and the body is
// intact (Open behavior unchanged).
func TestVerifyPasswordDoesNotConsumeBody(t *testing.T) {
	dir := t.TempDir()
	body := "URL: https://example.invalid/login"
	aesPath := filepath.Join(dir, "aes.zip")
	writeEncryptedEntry(t, aesPath, "secret", "a.txt", body, AES256Encryption)
	zcPath := filepath.Join(dir, "zc.zip")
	if err := writeZipCryptoEntry(zcPath, "secret", "a.txt", body, false); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{aesPath, zcPath} {
		file := firstFile(t, path)
		file.SetPassword("secret")
		if err := file.VerifyPassword(); err != nil {
			t.Fatalf("%s: VerifyPassword = %v, want nil", path, err)
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("%s: Open after verify = %v", path, err)
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("%s: read body = %v", path, err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("%s: close = %v", path, err)
		}
		if !bytes.Equal(got, []byte(body)) {
			t.Fatalf("%s: body = %q, want %q", path, got, body)
		}
	}
}
