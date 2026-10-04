package history_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// descending sizes: with several workers the small trailing units finish
// long before the big leading ones, so completion order differs from input
// order; FingerprintAll must still return slots indexed by input.
func fingerprintAllOrderPaths(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	sizes := []int{4 << 20, 3 << 20, 2 << 20, 1 << 20, 96, 64, 32, 16}
	paths := make([]string, len(sizes))
	for i, n := range sizes {
		p := filepath.Join(dir, "input-"+string(rune('a'+i))+".txt")
		writeFixture(t, p, strings.Repeat("x", n))
		paths[i] = p
	}
	return paths
}

func singleUnits(paths []string) []history.FingerprintUnit {
	units := make([]history.FingerprintUnit, len(paths))
	for i, p := range paths {
		units[i] = history.FingerprintUnit{Path: p}
	}
	return units
}

func TestFingerprintAllPreservesInputOrder(t *testing.T) {
	paths := fingerprintAllOrderPaths(t)
	cands, err := history.FingerprintAll(context.Background(), singleUnits(paths), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != len(paths) {
		t.Fatalf("got %d candidates, want %d", len(cands), len(paths))
	}
	for i, p := range paths {
		want, err := history.FingerprintFile(context.Background(), p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cands[i].ID != want.ID {
			t.Fatalf("slot %d = %v, want %v (input order broken)", i, cands[i].ID, want.ID)
		}
		if cands[i].Paths[0] != want.Paths[0] {
			t.Fatalf("slot %d path = %s, want %s", i, cands[i].Paths[0], want.Paths[0])
		}
	}
}

func TestFingerprintAllProgressTotals(t *testing.T) {
	paths := fingerprintAllOrderPaths(t)
	var total int64
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}

	var calls atomic.Int64
	var maxFiles int64
	var lastFiles, lastTotal int64
	var lastDone, lastBytes int64
	var mu sync.Mutex
	progress := func(filesDone, filesTotal int, bytesDone, bytesTotal int64) {
		mu.Lock()
		defer mu.Unlock()
		calls.Add(1)
		if int64(filesTotal) != int64(len(paths)) {
			t.Errorf("filesTotal = %d, want %d", filesTotal, len(paths))
		}
		if bytesTotal != total {
			t.Errorf("bytesTotal = %d, want %d", bytesTotal, total)
		}
		if int64(filesDone) > maxFiles {
			maxFiles = int64(filesDone)
		}
		lastFiles, lastTotal = int64(filesDone), int64(filesTotal)
		lastDone, lastBytes = bytesDone, bytesTotal
	}
	if _, err := history.FingerprintAll(context.Background(), singleUnits(paths), 4, progress); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() == 0 {
		t.Fatal("progress callback never invoked")
	}
	if maxFiles != int64(len(paths)) {
		t.Fatalf("max filesDone = %d, want %d", maxFiles, len(paths))
	}
	if lastFiles != int64(len(paths)) || lastDone != total {
		t.Fatalf("final progress = files %d/%d bytes %d/%d, want %d/%d %d/%d",
			lastFiles, lastTotal, lastDone, lastBytes, len(paths), len(paths), total, total)
	}
}

// A unit that fails mid-hash (stat OK, open denied) must cancel the other
// workers via context: the peer's bytes stop far short of its size, and the
// cause — not the peer's context.Canceled — is what comes back.
func TestFingerprintAllFirstErrorCancelsPeers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied open is unreachable as root")
	}
	// The sampled fingerprint reports a unit's full size after its first
	// 1 MiB sample, which completes in microseconds — the peer would finish
	// before the denied open errors, making the bytes-based cancellation
	// assertion vacuous. Inject read latency so the peer is still mid-sample
	// (paused, not yet progressed) when the first error cancels it.
	defer history.SetFingerprintReadPause(50 * time.Millisecond)()
	dir := t.TempDir()
	peer := filepath.Join(dir, "peer.txt")
	writeFixture(t, peer, strings.Repeat("x", 32<<20))
	peerSize := int64(32 << 20)

	bad := filepath.Join(dir, "denied.txt")
	writeFixture(t, bad, "x")
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}

	var lastBytes int64
	var mu sync.Mutex
	progress := func(filesDone, filesTotal int, bytesDone, bytesTotal int64) {
		mu.Lock()
		lastBytes = bytesDone
		mu.Unlock()
	}
	start := time.Now()
	_, err := history.FingerprintAll(context.Background(),
		[]history.FingerprintUnit{{Path: peer}, {Path: bad}}, 2, progress)
	if err == nil {
		t.Fatal("want the denied-open error, got nil")
	}
	if !strings.Contains(err.Error(), "denied.txt") {
		t.Fatalf("error = %v, want the denied.txt cause", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the underlying cause not context.Canceled", err)
	}
	mu.Lock()
	observed := lastBytes
	mu.Unlock()
	if observed >= peerSize {
		t.Fatalf("peer hashed %d/%d bytes: peers were not canceled after the first error",
			observed, peerSize)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("FingerprintAll took %v to surface the first error", elapsed)
	}
}

func TestFingerprintAllStatError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.txt")
	_, err := history.FingerprintAll(context.Background(),
		[]history.FingerprintUnit{{Path: missing}}, 0, nil)
	if err == nil {
		t.Fatal("want stat error for missing source, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want a wrapped fs.ErrNotExist", err)
	}
}

func TestFingerprintAllMultipartUnits(t *testing.T) {
	dir := t.TempDir()
	parts := multipartFixture(t, dir)
	single := filepath.Join(dir, "single.txt")
	writeFixture(t, single, "single")

	cands, err := history.FingerprintAll(context.Background(),
		[]history.FingerprintUnit{
			{Path: single},
			{Label: "archive", Volumes: parts},
		}, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSingle, err := history.FingerprintFile(context.Background(), single, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantMulti, err := history.FingerprintMultipart(context.Background(), "archive", parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].ID != wantSingle.ID || cands[0].Assembly != "" {
		t.Fatalf("slot 0 = %v, want single-file candidate", cands[0])
	}
	if cands[1].ID != wantMulti.ID || cands[1].Assembly != wantMulti.Assembly {
		t.Fatalf("slot 1 = %+v, want multipart candidate %+v", cands[1], wantMulti)
	}
}

func TestFingerprintAllTrivialInputs(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	writeFixture(t, empty, "")
	one := filepath.Join(dir, "one.txt")
	writeFixture(t, one, "one")

	// no units at all
	if cands, err := history.FingerprintAll(context.Background(), nil, 0, nil); err != nil || len(cands) != 0 {
		t.Fatalf("FingerprintAll(nil) = %v, %v", cands, err)
	}

	// workers > units, and workers <= 0 (default cap) both work
	for _, workers := range []int{99, 0, -3} {
		cands, err := history.FingerprintAll(context.Background(),
			[]history.FingerprintUnit{{Path: empty}, {Path: one}}, workers, nil)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if len(cands) != 2 || cands[0].ID.Size != 0 || cands[1].ID.Size != 3 {
			t.Fatalf("workers=%d: candidates = %+v", workers, cands)
		}
	}
}
