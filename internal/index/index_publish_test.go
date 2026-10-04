package index

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/searchidx"

	"github.com/klauspost/compress/zstd"
)

func mustZST(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestBuildDetectsArchiveSwapBetweenScanAndPublish covers RR-1.7: an archive
// atomically replaced between the scan and the publish must fail the identity
// check with ErrArchiveChanged and leave no sidecar behind.
func TestBuildDetectsArchiveSwapBetweenScanAndPublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	orig := mustZST(t, []byte("hello world\n"))
	other := mustZST(t, []byte("entirely different content\n"))
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	restore := afterScanForTest
	defer func() { afterScanForTest = restore }()
	afterScanForTest = func(p string) {
		// Atomic replacement: new inode now owns the archive pathname.
		tmp := p + ".swap"
		if err := os.WriteFile(tmp, other, 0o644); err != nil {
			t.Errorf("write swap archive: %v", err)
			return
		}
		if err := os.Rename(tmp, p); err != nil {
			t.Errorf("swap archive: %v", err)
		}
	}

	_, err := Build(context.Background(), path, nil, nil)
	if !errors.Is(err, ErrArchiveChanged) {
		t.Fatalf("err = %v, want ErrArchiveChanged", err)
	}
	if _, serr := os.Stat(searchidx.WriteSidecarPath(path)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar must not be published after archive swap (stat err = %v)", serr)
	}
}

// TestBuildDetectsInPlaceRewriteBeforePublish: a same-inode rewrite is still
// caught when it changes size or mtime before the publish check.
func TestBuildDetectsInPlaceRewriteBeforePublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	orig := mustZST(t, []byte("hello world\n"))
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	restore := afterScanForTest
	defer func() { afterScanForTest = restore }()
	afterScanForTest = func(p string) {
		// Same inode, rewritten in place to a different length.
		if err := os.WriteFile(p, []byte("rewritten in place\n"), 0o644); err != nil {
			t.Errorf("rewrite archive: %v", err)
		}
	}

	_, err := Build(context.Background(), path, nil, nil)
	if !errors.Is(err, ErrArchiveChanged) {
		t.Fatalf("err = %v, want ErrArchiveChanged", err)
	}
	if _, serr := os.Stat(searchidx.WriteSidecarPath(path)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar must not be published after rewrite (stat err = %v)", serr)
	}
}
