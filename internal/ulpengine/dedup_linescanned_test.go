package ulpengine

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// LinesScanned must count EVERY record that reaches the dedup lookup path —
// dest-set hits, in-run seen-map dupes and uniques alike — while skipping the
// n==0 empty-header records that continue before any lookup.
func TestLinesScannedCountsLookupPath(t *testing.T) {
	dir := t.TempDir()

	// library sidecar holding exactly one key: 0xDEADBEEF
	libArch := filepath.Join(dir, "sfu_lib.txt.zst")
	writeSidecarKeysForTest(t, libArch, []uint64{0xDEADBEEF})
	destSidecar := sidecarPathForArchive(libArch)

	bucketPath := filepath.Join(dir, defaultBucketName(0))
	writeBucket(t, bucketPath, []bucketRecord{
		{hash: 0xDEADBEEF, line: "lib.example.com:u:hit"}, // dest hit
		{hash: 1, line: "a.example.com:u:one"},            // unique
		{hash: 2, line: "b.example.com:u:two"},            // unique (first-seen)
		{hash: 2, line: "b.example.com:u:two"},            // in-run dupe (same strong ID)
		{hash: 3, line: ""},                               // n==0 empty header: NOT scanned
	})

	out := filepath.Join(dir, "out.txt")
	m := &Metrics{}
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := dedup(context.Background(), dedupConfig{
		bucketPaths:  []string{bucketPath},
		destSidecars: []string{destSidecar},
		workers:      1,
	}, sink, m)
	if err != nil {
		_ = sink.abort()
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}

	// 4 records reach the lookup path: dest hit + 3 hash scans (the n==0
	// header-only record continues before any lookup).
	if got := m.LinesScanned.Load(); got != 4 {
		t.Errorf("LinesScanned = %d, want 4 (dest hit + uniques + in-run dupes, no empty-header record)", got)
	}
	if got := m.LinesSkippedByDest.Load(); got != 1 {
		t.Errorf("LinesSkippedByDest = %d, want 1", got)
	}
	if got := m.LinesUnique.Load(); got != 2 {
		t.Errorf("LinesUnique = %d, want 2", got)
	}
	if n != 2 {
		t.Errorf("unique lines written = %d, want 2", n)
	}
}

// During the per-bucket library-key gather (gatherDestBucketKeys) NO record
// has reached the lookup loop yet, so LinesScanned legitimately stays 0 —
// not a lost increment. Live-observed on a huge library (120M of 3.8B keys
// gathered after 39 s, buckets 0/256 done, all workers busy gathering).
// Pins that gather progress (KeysLoaded climbs) while LinesScanned is still
// zero.
func TestLinesScannedZeroDuringDestGather(t *testing.T) {
	dir := t.TempDir()

	libArch := filepath.Join(dir, "sfu_lib.txt.zst")
	keys := make([]uint64, 64)
	for i := range keys {
		keys[i] = uint64(0x1000 + i)
	}
	writeSidecarKeysForTest(t, libArch, keys)
	destSidecar := sidecarPathForArchive(libArch)

	bucketPath := filepath.Join(dir, defaultBucketName(0))
	writeBucket(t, bucketPath, []bucketRecord{
		{hash: 0x1000, line: "lib.example.com:u:hit"},
		{hash: 1, line: "a.example.com:u:one"},
	})

	odm := &ODMetrics{}
	m := &Metrics{}
	var observedGather atomic.Bool

	prevHook := gatherPollHook
	gatherPollHook = func() {
		if odm.KeysLoaded.Load() > 0 {
			observedGather.Store(true)
			if got := m.LinesScanned.Load(); got != 0 {
				t.Errorf("LinesScanned = %d during dest gather, want 0", got)
			}
		}
	}
	defer func() { gatherPollHook = prevHook }()

	out := filepath.Join(dir, "out.txt")
	sink, err := newOutputSink(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = dedup(context.Background(), dedupConfig{
		bucketPaths:  []string{bucketPath},
		destSidecars: []string{destSidecar},
		workers:      1,
		odMetrics:    odm,
	}, sink, m)
	if err != nil {
		_ = sink.abort()
		t.Fatalf("dedup: %v", err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	if !observedGather.Load() {
		t.Error("gather poll hook never observed loaded keys; the gather window was not exercised")
	}
	if got := m.LinesScanned.Load(); got != 2 {
		t.Errorf("LinesScanned = %d after dedup, want 2 (dest hit + unique)", got)
	}
}
