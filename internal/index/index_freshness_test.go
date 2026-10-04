package index_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/searchidx"

	"github.com/klauspost/compress/zstd"
)

func zstdBytes(t *testing.T, data []byte) []byte {
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

// sameSizeBytes returns a compressed archive of the same byte length as want
// but built from different content, proving size alone cannot detect rewrites.
// The payload grows by high-entropy filler characters, so compressed sizes
// sweep the whole band and the exact target length is always reachable.
func sameSizeBytes(t *testing.T, want []byte) []byte {
	t.Helper()
	const hexDigits = "0123456789abcdef"
	for n := 0; n < 65536; n++ {
		filler := make([]byte, n)
		x := uint64(n)*2654435761 + 0x9E3779B97F4A7C15
		for j := range filler {
			x = x*6364136223846793005 + 1442695040888963407
			filler[j] = hexDigits[x>>33&15]
		}
		payload := append([]byte("alpha line one\nneedle swapped "), filler...)
		payload = append(payload, '\n')
		got := zstdBytes(t, payload)
		if len(got) == len(want) && !bytes.Equal(got, want) {
			return got
		}
	}
	t.Fatal("no same-size different-content archive found")
	return nil
}

// TestEnsureRebuildsAfterSameSizeSameMtimeRewrite covers RR-1.6: an archive
// rewritten to different bytes of equal size with a restored mtime must be
// detected by the recorded digest and rebuilt, never searched with stale
// offsets.
func TestEnsureRebuildsAfterSameSizeSameMtimeRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	orig := zstdBytes(t, []byte("alpha line one\nneedle original\n"))
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta.Action)
	}
	origMod := sc.SourceIdentity.ModTimeUnixNano

	rewritten := sameSizeBytes(t, orig)
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(0, origMod), time.Unix(0, origMod)); err != nil {
		t.Fatal(err)
	}

	stale, err := index.IsStale(context.Background(), path, searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale sidecar after equal-size equal-mtime rewrite")
	}

	sc2, meta2, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("ensure after rewrite: %v", err)
	}
	if meta2.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta2.Action)
	}
	sum := sha256.Sum256(rewritten)
	if sc2.SourceIdentity.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("rebuilt sidecar digest does not describe the rewritten content")
	}
	if sc2.SourceIdentity.Size != int64(len(rewritten)) {
		t.Fatalf("recorded size = %d, want %d", sc2.SourceIdentity.Size, len(rewritten))
	}

	// Unchanged content stays fresh: no rebuild on the third run.
	_, meta3, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta3.Action != index.EnsureActionLoad {
		t.Fatalf("action = %q, want load", meta3.Action)
	}
}

// TestEnsureRebuildsV1Sidecar covers RR-1.8's format bump: a v1 sidecar is
// unsupported derived data and must be rebuilt automatically into v2 with a
// source identity, never migrated in place.
func TestEnsureRebuildsV1Sidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	if err := os.WriteFile(path, zstdBytes(t, []byte("hello world\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	v1 := `{
  "version": 1,
  "format": "snowfastsearch.idx.v1",
  "source": "sample.zst",
  "chunks": [
    {
      "chunk_id": 0,
      "compressed_offset": 0,
      "compressed_size": 10,
      "uncompressed_start": 0,
      "uncompressed_end": 12
    }
  ]
}`
	lib := searchidx.LibrarySidecarPath(path)
	if err := os.MkdirAll(filepath.Dir(lib), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lib, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatalf("ensure with v1 sidecar: %v", err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta.Action)
	}
	if sc.Format != searchidx.FormatName || sc.Version != searchidx.FormatVersion {
		t.Fatalf("rebuilt sidecar = %s v%d, want %s v%d", sc.Format, sc.Version, searchidx.FormatName, searchidx.FormatVersion)
	}
	if sc.SourceIdentity.SHA256 == "" || sc.SourceIdentity.Size == 0 {
		t.Fatalf("rebuilt sidecar missing source identity: %+v", sc.SourceIdentity)
	}

	// The rebuilt v2 sidecar loads cleanly afterwards.
	if _, err := index.Load(lib); err != nil {
		t.Fatalf("reload rebuilt sidecar: %v", err)
	}
}

// TestEnsureV2SidecarRequiresIdentity: well-formed v2 JSON without a source
// identity must not be accepted as fresh.
func TestEnsureV2SidecarRequiresIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	if err := os.WriteFile(path, zstdBytes(t, []byte("hello world\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Build(context.Background(), path, nil, nil); err != nil {
		t.Fatal(err)
	}
	lib := searchidx.LibrarySidecarPath(path)
	noIdentity := `{"version":2,"format":"snowfastsearch.idx.v2","source":"sample.zst","chunks":[{"chunk_id":0,"compressed_offset":0,"compressed_size":10,"uncompressed_start":0,"uncompressed_end":12}]}`
	if err := os.WriteFile(lib, []byte(noIdentity), 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := index.Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Action != index.EnsureActionBuild {
		t.Fatalf("action = %q, want build", meta.Action)
	}
}

// TestEnsureConcurrentBuildsConverge covers RR-1.8's race fix: two concurrent
// Ensure calls on the same archive must both succeed and converge on one
// valid sidecar with no temp residue.
func TestEnsureConcurrentBuildsConverge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	data := zstdBytes(t, []byte("alpha line\nbeta needle line\ngamma\n"))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	results := make([]EnsureResult, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			sc, meta, err := index.Ensure(context.Background(), path, nil, nil)
			results[i] = EnsureResult{sc: sc, meta: meta, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("ensure %d: %v", i, r.err)
		}
		if r.sc == nil || len(r.sc.Chunks) != 1 {
			t.Fatalf("ensure %d returned unusable sidecar", i)
		}
	}

	// Exactly one valid published sidecar, no temp residue.
	entries, err := os.ReadDir(filepath.Dir(searchidx.WriteSidecarPath(path)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp residue after concurrent builds: %s", e.Name())
		}
	}
	loaded, err := index.Load(searchidx.WriteSidecarPath(path))
	if err != nil {
		t.Fatalf("load converged sidecar: %v", err)
	}
	sum := sha256.Sum256(data)
	if loaded.SourceIdentity.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("converged sidecar does not describe the archive content")
	}
}

type EnsureResult struct {
	sc   *index.Sidecar
	meta index.EnsureMeta
	err  error
}

// TestEnsurePropagatesBuildErrors: a failing build (here: cancellation) must
// surface its real error and publish nothing.
func TestEnsurePropagatesBuildErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	if err := os.WriteFile(path, zstdBytes(t, []byte("hello world\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := index.Ensure(ctx, path, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, serr := os.Stat(searchidx.WriteSidecarPath(path)); !os.IsNotExist(serr) {
		t.Fatalf("sidecar must not be published on failure (stat err = %v)", serr)
	}
}
