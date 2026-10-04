package zstdframe_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
)

// TestScanFileWithIdentity verifies the identity binds the scan to the exact
// descriptor: same file as the pathname's current stat, digest equal to the
// content's SHA-256.
func TestScanFileWithIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.zst")
	payload := []byte("alpha line\nbeta needle line\ngamma\n")
	writeZST(t, path, payload)

	frames, ident, err := zstdframe.ScanFileWithIdentity(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	sum := sha256.Sum256(mustReadFile(t, path))
	if ident.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("identity digest = %s, want %s", ident.SHA256, hex.EncodeToString(sum[:]))
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(st, ident.Stat) {
		t.Fatal("identity stat does not describe the archive file")
	}
	if ident.Stat.Size() != st.Size() {
		t.Fatalf("identity size = %d, want %d", ident.Stat.Size(), st.Size())
	}
}

// TestScanFileWithIdentityEmptyFile: a zero-byte archive still yields a valid
// identity (empty-content digest) and no frames.
func TestScanFileWithIdentityEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.zst")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	frames, ident, err := zstdframe.ScanFileWithIdentity(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatalf("frames = %d, want 0", len(frames))
	}
	sum := sha256.Sum256(nil)
	if ident.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("empty digest = %s", ident.SHA256)
	}
}

// TestHashFileCancel: HashFile honors ctx cancellation between buffer reads.
func TestHashFileCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := zstdframe.HashFile(ctx, f); err == nil {
		t.Fatal("expected cancel error from HashFile")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
