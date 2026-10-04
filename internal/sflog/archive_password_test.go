package sflog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	zipenc "github.com/yeka/zip"
)

// corruptAESMemberBodyAfterPVV rewrites everything between the salt+PVV and
// the trailing auth code with garbage, leaving the encryption header (the only
// bytes a header-only probe may read) intact. If resolveZipPassword ever
// decompressed the probe body, the correct password would fail on this member.
func corruptAESMemberBodyAfterPVV(t *testing.T, path, member string) {
	t.Helper()
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var target *zipenc.File
	for _, f := range zr.File {
		if f.Name == member {
			target = f
		}
	}
	if target == nil {
		t.Fatalf("member %q not found", member)
	}
	off, err := target.DataOffset()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	saltLen := 16 + 2 // AES-256: salt = keyLen/2 = 16, plus 2-byte PVV
	junk := make([]byte, int(target.CompressedSize64)-saltLen-10)
	for i := range junk {
		junk[i] = 0xA5
	}
	if _, err := f.WriteAt(junk, off+int64(saltLen)); err != nil {
		t.Fatal(err)
	}
}

// resolveZipPassword must surface context cancellation as its own error, not
// as password-not-found.
func TestResolveZipPasswordCancellationDistinct(t *testing.T) {
	path := "testdata/resolve-cancel.zip"
	writeEncryptedTestZipMethod(t, path, "secret", "victim/Passwords.txt",
		"URL: https://x.example/login\nUSER: u\nPASS: p\n", zipenc.AES256Encryption)
	defer os.Remove(path)

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pw, ok, err := resolveZipPassword(ctx, zr.File[0], []string{"a", "b"})
	if ok || pw != "" {
		t.Fatalf("cancelled resolution reported a password: (%q, %v)", pw, ok)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolution error = %v, want context.Canceled", err)
	}

	// Contrast: a clean context with no matching candidate is not-found, nil.
	pw, ok, err = resolveZipPassword(context.Background(), zr.File[0], []string{"a", "b"})
	if err != nil || ok {
		t.Fatalf("not-found resolution = (%q, %v, %v), want (\"\", false, nil)", pw, ok, err)
	}
}

// ZipCrypto (legacy) members resolve through the 12-byte header check byte:
// the correct password verifies, wrong candidates do not.
func TestResolveZipPasswordZipCryptoMember(t *testing.T) {
	path := "testdata/resolve-zipcrypto.zip"
	writeDiscriminatingZipCryptoZip(t, path, "secret", "victim/Passwords.txt",
		"URL: https://x.example/login\nUSER: u\nPASS: p\n", "nope")
	defer os.Remove(path)

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	pw, ok, err := resolveZipPassword(context.Background(), zr.File[0], []string{"nope", "secret"})
	if err != nil {
		t.Fatalf("resolveZipPassword error = %v", err)
	}
	if !ok || pw != "secret" {
		t.Fatalf("resolved = (%q, %v), want (\"secret\", true)", pw, ok)
	}
}

func TestCorruptedBodyFixtureIsReallyBroken(t *testing.T) {
	path := "testdata/resolve-probe-broken.zip"
	writeEncryptedTestZipMethod(t, path, "secret", "victim/Passwords.txt",
		"URL: https://x.example/login\nUSER: u\nPASS: p\n", zipenc.AES256Encryption)
	defer os.Remove(path)
	corruptAESMemberBodyAfterPVV(t, path, "victim/Passwords.txt")

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	zr.File[0].SetPassword("secret")
	rc, err := zr.File[0].Open()
	if err != nil {
		return // open-time rejection is fine
	}
	_, err = io.Copy(io.Discard, rc)
	rc.Close()
	if err == nil {
		t.Fatal("garbage-body fixture decoded cleanly; corruption did not apply")
	}
}

// findZipCryptoCheckByteCollision writes a ZipCrypto fixture encrypted with
// "secret" and scans a deterministic generated wordlist for a WRONG candidate
// whose 12-byte header check byte coincides (1/256 per candidate). The
// resulting pair is the false positive RR-1.14 guards against: the wrong
// candidate passes VerifyPassword but its decrypted body fails checksum.
func findZipCryptoCheckByteCollision(t *testing.T, path string) string {
	t.Helper()
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for i := range 4096 {
		cand := fmt.Sprintf("false-%04d", i)
		zr.File[0].SetPassword(cand)
		if err := zr.File[0].VerifyPassword(); err == nil {
			return cand
		}
	}
	t.Fatal("no check-byte collision found in 4096 deterministic candidates; fixture is broken")
	return ""
}

// A wrong ZipCrypto candidate passes the 1-byte header check (1/256 fluke) but
// its body cannot decode: resolveZipPassword must confirm candidates by
// streaming the probe member through checksummed decompression, so the real
// password later in the wordlist still wins instead of the sweep stopping on
// the false positive.
func TestResolveZipPasswordZipCryptoCheckByteFalsePositive(t *testing.T) {
	path := "testdata/resolve-false-positive.zip"
	// Fixed DOS time keeps the archive byte-identical across runs, so the
	// collision candidate is deterministic.
	writeEncryptedTestZipMethodAt(t, path, "secret", "victim/Passwords.txt",
		strings.Repeat("URL: https://x.example/login\nUSER: u\nPASS: p\n", 40),
		zipenc.StandardEncryption, 0x1200)
	defer os.Remove(path)
	colliding := findZipCryptoCheckByteCollision(t, path)

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	pw, ok, err := resolveZipPassword(context.Background(), zr.File[0], []string{colliding, "secret"})
	if err != nil {
		t.Fatalf("resolveZipPassword error = %v", err)
	}
	if !ok || pw != "secret" {
		t.Fatalf("resolved = (%q, %v), want the real password (%q, true): the check-byte false positive must not stop the sweep", pw, ok, "secret")
	}
	// The correct password alone resolves cleanly through the same body check.
	pw, ok, err = resolveZipPassword(context.Background(), zr.File[0], []string{"secret"})
	if err != nil || !ok || pw != "secret" {
		t.Fatalf("correct-password resolution = (%q, %v, %v), want (%q, true, nil)", pw, ok, err, "secret")
	}
}

// AES candidates share the same body-confirmation path: a candidate whose
// verification value passes but whose body fails the auth code / checksum is a
// false positive and must be rejected, not returned. (The correct password on
// an intact archive still resolves; the probe body is decoded twice, which is
// the accepted cost.)
func TestResolveZipPasswordRejectsBodyCorruptAESCandidate(t *testing.T) {
	path := "testdata/resolve-aes-corrupt-body.zip"
	writeEncryptedTestZipMethod(t, path, "secret", "victim/Passwords.txt",
		"URL: https://x.example/login\nUSER: u\nPASS: p\n", zipenc.AES256Encryption)
	defer os.Remove(path)
	corruptAESMemberBodyAfterPVV(t, path, "victim/Passwords.txt")

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	pw, ok, err := resolveZipPassword(context.Background(), zr.File[0], []string{"secret"})
	if err != nil {
		t.Fatalf("resolveZipPassword error = %v", err)
	}
	if ok || pw != "" {
		t.Fatalf("corrupt-body candidate resolved = (%q, %v), want rejection (\"\", false)", pw, ok)
	}
}

// probeBodyDecodes must stop immediately on context cancellation rather than
// streaming the member to EOF.
func TestProbeBodyDecodesHonorsCancellation(t *testing.T) {
	path := "testdata/resolve-probe-cancel.zip"
	writeEncryptedTestZipMethod(t, path, "secret", "victim/Passwords.txt",
		"URL: https://x.example/login\nUSER: u\nPASS: p\n", zipenc.AES256Encryption)
	defer os.Remove(path)

	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	zr.File[0].SetPassword("secret")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probeBodyDecodes(ctx, zr.File[0]); !errors.Is(err, context.Canceled) {
		t.Fatalf("probeBodyDecodes cancelled err = %v, want context.Canceled", err)
	}
	if err := probeBodyDecodes(context.Background(), zr.File[0]); err != nil {
		t.Fatalf("probeBodyDecodes intact-body err = %v, want nil", err)
	}
}
