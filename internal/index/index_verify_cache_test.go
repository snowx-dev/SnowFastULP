package index

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/searchidx"
)

// writeTestArchive writes a small zst archive (the external test package has
// its own zstdBytes/writeZST variants; this file is package index).
func writeTestArchive(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, mustZST(t, []byte(content)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureSteadyStateSkipsDigestPass pins the Verify-cache contract: the
// first post-build Ensure performs exactly one backfill digest pass, and
// every later Ensure on the unchanged archive performs none.
func TestEnsureSteadyStateSkipsDigestPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeTestArchive(t, path, "hello world\n")

	hashCountForTest.Store(0)
	_, meta, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Action != EnsureActionBuild {
		t.Fatalf("first ensure action = %q, want build", meta.Action)
	}
	if n := hashCountForTest.Load(); n != 0 {
		t.Fatalf("build ran %d hashArchive passes, want 0 (the scan hashes internally)", n)
	}

	// First sight of this archive instance by the freshness check: one
	// digest pass, then the confirmation is persisted in the sidecar.
	_, meta2, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta2.Action != EnsureActionLoad {
		t.Fatalf("second ensure action = %q, want load", meta2.Action)
	}
	if n := hashCountForTest.Load(); n != 1 {
		t.Fatalf("second ensure hashArchive passes = %d, want 1 (backfill)", n)
	}

	// Steady state: no digest pass at all.
	_, meta3, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta3.Action != EnsureActionLoad {
		t.Fatalf("third ensure action = %q, want load", meta3.Action)
	}
	if n := hashCountForTest.Load(); n != 1 {
		t.Fatalf("third ensure hashArchive passes = %d, want still 1", n)
	}
}

func TestEnsureRefreshesSidecarModAfterVerifyBackfill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeTestArchive(t, path, "hello world\n")

	_, built, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if built.Action != EnsureActionBuild {
		t.Fatalf("first ensure action = %q, want build", built.Action)
	}

	oldMod := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(built.SidecarPath, oldMod, oldMod); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(built.SidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	oldMod = before.ModTime()

	_, loaded, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Action != EnsureActionLoad {
		t.Fatalf("backfill ensure action = %q, want load", loaded.Action)
	}
	if loaded.SidecarMod.Equal(oldMod) {
		t.Fatalf("sidecar mtime was not refreshed after Verify backfill: got pre-backfill %s", loaded.SidecarMod)
	}
	after, err := os.Stat(built.SidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.SidecarMod.Equal(after.ModTime()) {
		t.Fatalf("reported sidecar mtime = %s, want on-disk mtime %s", loaded.SidecarMod, after.ModTime())
	}
}

// TestEnsureRehashesOncePerArchiveInstance: touching the archive (new mtime)
// rebuilds; the next Ensure re-confirms the digest exactly once and steady
// state resumes.
func TestEnsureRehashesOncePerArchiveInstance(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeTestArchive(t, path, "hello world\n")

	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil { // backfill
		t.Fatal(err)
	}
	hashCountForTest.Store(0)

	if err := os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, meta, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Action != EnsureActionBuild {
		t.Fatalf("ensure after touch action = %q, want build", meta.Action)
	}
	if n := hashCountForTest.Load(); n != 0 {
		t.Fatalf("touch-rebuild hashArchive passes = %d, want 0 (stale at identity check)", n)
	}

	_, meta2, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta2.Action != EnsureActionLoad {
		t.Fatalf("post-rebuild ensure action = %q, want load", meta2.Action)
	}
	if n := hashCountForTest.Load(); n != 1 {
		t.Fatalf("post-rebuild backfill hashArchive passes = %d, want 1", n)
	}

	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := hashCountForTest.Load(); n != 1 {
		t.Fatalf("steady-state hashArchive passes after backfill = %d, want still 1", n)
	}
}

// TestVerifyTracksArchiveInstance: an atomic replacement with identical
// content and a copied mtime presents a new instance token (inode); the cache
// must re-run the digest pass once for the new instance and re-record the
// Verify block, then return to steady state.
//
// Unix only: on Windows the instance token is empty by design, so a
// same-size rename replacement with a restored mtime is accepted without a
// digest pass (see archiveInstanceToken for the documented platform
// difference).
func TestVerifyTracksArchiveInstance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("instance token (dev:ino) is a unix concept; see archiveInstanceToken")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	writeTestArchive(t, path, "hello world\n")

	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil { // backfill
		t.Fatal(err)
	}
	sc1, err := Load(searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if sc1.Verify == nil {
		t.Fatal("backfilled sidecar missing Verify block")
	}
	origMod := sc1.SourceIdentity.ModTimeUnixNano
	token1 := sc1.Verify.InodeOrHandle
	if token1 == "" {
		t.Fatal("instance token must be non-empty on this platform")
	}

	// Atomic replacement with identical bytes; restore the recorded mtime so
	// size and mtime still match but the instance (inode) differs.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".replaced"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(0, origMod), time.Unix(0, origMod)); err != nil {
		t.Fatal(err)
	}

	hashCountForTest.Store(0)
	_, meta, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Action != EnsureActionLoad {
		t.Fatalf("ensure after same-content replacement action = %q, want load", meta.Action)
	}
	if n := hashCountForTest.Load(); n != 1 {
		t.Fatalf("new-instance hashArchive passes = %d, want 1 (token changed)", n)
	}
	sc2, err := Load(searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if sc2.Verify == nil {
		t.Fatal("re-recorded sidecar missing Verify block")
	}
	if sc2.Verify.InodeOrHandle == token1 {
		t.Fatal("Verify block was not re-recorded for the new archive instance")
	}

	// Steady state again for the new instance.
	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if n := hashCountForTest.Load(); n != 1 {
		t.Fatalf("steady-state hashArchive passes after re-record = %d, want still 1", n)
	}
}

// TestSameInstanceForgedMtimeSwapContract pins the RR-1.6 contract as it
// moves to the Verify cache: an equal-size rewrite is detected when
// (size, mtime, inode) differs from the last verified identity.
//
// Documented residual: once the Verify block is recorded, an in-place
// same-size rewrite whose mtime is forged back to the verified value on the
// same file instance is indistinguishable from the verified content by stat
// alone and is accepted as fresh (hashCount stays put). The digest still
// guards first sight of every new (size, mtime) pair and every new instance
// token; a rewrite that does not forge the mtime is always caught.
func TestSameInstanceForgedMtimeSwapContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.zst")
	orig := mustZST(t, []byte("alpha line one\nneedle original\n"))
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Ensure(context.Background(), path, nil, nil); err != nil { // backfill
		t.Fatal(err)
	}
	sc, err := Load(searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	origMod := sc.SourceIdentity.ModTimeUnixNano

	// Swap 1: equal size, forged old mtime, same inode, Verify recorded.
	rewritten := sameSizeExternal(t, orig)
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(0, origMod), time.Unix(0, origMod)); err != nil {
		t.Fatal(err)
	}
	hashCountForTest.Store(0)
	_, meta, err := Ensure(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Action != EnsureActionLoad {
		t.Fatalf("forged-mtime same-instance swap action = %q, want load (documented residual)", meta.Action)
	}
	if n := hashCountForTest.Load(); n != 0 {
		t.Fatalf("forged-mtime same-instance swap hash passes = %d, want 0 (documented residual)", n)
	}

	// Swap 2: same rewrite, but the mtime is allowed to change — must be
	// caught by the identity check and rebuilt.
	if err := os.Chtimes(path, time.Unix(0, origMod+1), time.Unix(0, origMod+1)); err != nil {
		t.Fatal(err)
	}
	stale, err := IsStale(context.Background(), path, searchidx.LibrarySidecarPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if !stale {
		t.Fatal("expected stale sidecar after same-size swap with new mtime")
	}
}

// sameSizeExternal mirrors index_freshness_test.go's sameSizeBytes (unexported
// there; this file is package index).
func sameSizeExternal(t *testing.T, want []byte) []byte {
	t.Helper()
	const hexDigits = "0123456789abcdef"
	for n := range 65536 {
		filler := make([]byte, n)
		x := uint64(n)*2654435761 + 0x9E3779B97F4A7C15
		for j := range filler {
			x = x*6364136223846793005 + 1442695040888963407
			filler[j] = hexDigits[x>>33&15]
		}
		payload := append([]byte("alpha line one\nneedle swapped "), filler...)
		payload = append(payload, '\n')
		got := mustZST(t, payload)
		if len(got) == len(want) && !bytes.Equal(got, want) {
			return got
		}
	}
	t.Fatal("no same-size different-content archive found")
	return nil
}
