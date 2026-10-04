package ulpengine

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
)

// TestDedupDestSidecarsFDLimitNoEMFILE: gatherDestBucketKeys must open ONE
// dest sidecar at a time. Under a deliberately low RLIMIT_NOFILE, a full
// parallel dedup against a 24-part library must succeed — the old
// worker-lifetime reader map (workers × parts open handles) would exhaust
// descriptors and fail with EMFILE.
func TestDedupDestSidecarsFDLimitNoEMFILE(t *testing.T) {
	lowerFDLimit(t, 28)

	d := t.TempDir()
	const numParts = 24
	destSidecars := make([]string, 0, numParts)
	for i := 0; i < numParts; i++ {
		arch := filepath.Join(d, fmt.Sprintf("sfu_20260920_fd_part%d.txt.zst", i+1))
		writeSidecarKeysForTest(t, arch, []uint64{
			uint64(i)*1000 + 1,
			uint64(i)*1000 + 2,
		})
		destSidecars = append(destSidecars, sidecarPathForArchive(arch))
	}

	const B = 4
	const perBucket = 10
	bucketPaths := make([]string, B)
	for b := 0; b < B; b++ {
		p := filepath.Join(d, defaultBucketName(b))
		recs := make([]bucketRecord, 0, perBucket)
		for i := 0; i < perBucket; i++ {
			h := (uint64(b) << 40) | (1 << 62) | uint64(i) // distinct, not in library keys
			line := "fdtest" + strconv.Itoa(b) + ".example.com:u:" + strconv.Itoa(i)
			recs = append(recs, bucketRecord{hash: h, line: line})
		}
		writeBucket(t, p, recs)
		bucketPaths[b] = p
	}

	out := filepath.Join(d, "out.txt")
	m := &Metrics{}
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	_, derr := dedup(context.Background(), dedupConfig{
		bucketPaths:  bucketPaths,
		destSidecars: destSidecars,
		workers:      4,
	}, sink, m)
	if derr != nil {
		_ = sink.abort()
		t.Fatalf("dedup under low fd limit: %v", derr)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
}
