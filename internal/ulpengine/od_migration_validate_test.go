package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestBucketKeysFailsLoudOnTruncatedSidecar guards the fix that a short read of
// a sidecar bucket is a hard error rather than a silent zero-fill (which would
// inject a spurious key 0 into the dest set). Simulates the file being
// truncated out from under an already-open reader (post-validation TOCTOU).
func TestBucketKeysFailsLoudOnTruncatedSidecar(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_trunc.txt.zst")
	writeSidecarKeysForTest(t, archive, []uint64{3, 1, 4, 1, 5, 9, 2, 6, 8, 7})
	path := sidecarPathForArchive(archive)

	sr, err := openSidecarReader(path) // header validates body size here
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer sr.close()

	// Lop off the last key's bytes after the reader cached keyCount: the bulk
	// read now spans past EOF.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, fi.Size()-SidecarKeyBytes); err != nil {
		t.Fatal(err)
	}

	// Single bucket → range + block decode → short read fails loud.
	var readErr error
	lo, hi, rerr := sr.bucketRange(context.Background(), 0, 1)
	if rerr != nil {
		readErr = rerr
	} else {
		keys := make([]uint64, hi-lo)
		readErr = sr.decodeBucketRange(context.Background(), keys, lo)
	}
	if readErr == nil {
		t.Fatal("bucket range read silently tolerated a truncated sidecar (want a hard error)")
	}
}
