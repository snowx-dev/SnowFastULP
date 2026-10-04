package ulpengine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// durability event: kind is "sync-file" or "sync-dir", path the fsync target.
// The hooks are test-only seams (nil in production).
type durableEvent struct {
	kind string
	path string
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return err == nil
}

// installDurableHooks wires the test-only durability seams and restores them
// at cleanup.
func installDurableHooks(t *testing.T, rec *[]durableEvent,
	failFileSync, failDirSync func(path string) bool) {
	t.Helper()
	origFile, origDir, origEvent := durableSyncFileHook, durableSyncDirHook, durableEventHook
	durableSyncFileHook = func(path string) error {
		if failFileSync != nil && failFileSync(path) {
			return &os.PathError{Op: "fsync", Path: path, Err: os.ErrInvalid}
		}
		return nil
	}
	durableSyncDirHook = func(path string) error {
		if failDirSync != nil && failDirSync(path) {
			return &os.PathError{Op: "fsync-dir", Path: path, Err: os.ErrInvalid}
		}
		return nil
	}
	durableEventHook = func(kind, path string) {
		*rec = append(*rec, durableEvent{kind, path})
	}
	t.Cleanup(func() { durableSyncFileHook, durableSyncDirHook, durableEventHook = origFile, origDir, origEvent })
}

// TestDurablePublishOrderSingleSink: seal must fsync the archive temp and
// every sidecar temp before close/rename; commit must rename first and sync
// each immediate parent directory only after its final rename.
func TestDurablePublishOrderSingleSink(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "sfu_dur.txt.zst")
	sink, err := newOutputSinkWithSidecar(out, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if werr := sink.writeBatchIndexed([]byte("h1.example.com:u:p1\nh2.example.com:u:p2\n"),
		[]uint64{11, 22}, 2, nil); werr != nil {
		t.Fatal(werr)
	}

	var events []durableEvent
	installDurableHooks(t, &events, nil, nil)

	if serr := sink.seal(); serr != nil {
		t.Fatalf("seal: %v", serr)
	}
	if cerr := sink.commit(); cerr != nil {
		t.Fatalf("commit: %v", cerr)
	}

	files := filterEvents(events, "sync-file")
	dirs := filterEvents(events, "sync-dir")

	// seal: exactly the three staged temps get file syncs, while every final
	// name is still untouched (nothing published before commit).
	if len(files) != 3 {
		t.Fatalf("sync-file events = %d (%v), want 3 staged temps", len(files), files)
	}
	for _, e := range files {
		if exists(t, e.path) && !isTempOf(e.path, sink) {
			t.Errorf("sync-file on non-temp path %s", e.path)
		}
	}
	if len(dirs) == 0 {
		t.Fatal("no parent-directory syncs recorded")
	}

	// commit: each sync-dir event must observe the final it covers already
	// renamed into place — rename BEFORE parent sync.
	for _, e := range dirs {
		if e.path == d && !exists(t, out) {
			t.Fatalf("archive dir synced before final rename of %s: %v", out, e)
		}
		if e.path == filepath.Join(d, idxSubdirName) && !exists(t, sidecarPathForArchive(out)) {
			t.Fatalf("idx dir synced before final sidecar rename: %v", e)
		}
	}
	// ordering: all file syncs happen before any directory sync (seal < commit)
	if len(dirs) > 0 && eventIndex(events, dirs[0]) < eventIndex(events, files[len(files)-1]) {
		t.Fatalf("parent sync before file syncs: %v", events)
	}

	// every staged temp was consumed: none of the sync'd temp paths survive
	for _, e := range files {
		if _, err := os.Stat(e.path); !os.IsNotExist(err) {
			t.Errorf("staged temp %s survived commit", e.path)
		}
	}
}

// isTempOf reports whether p is one of the sink's staged temps (archive, idx,
// search sidecar).
func isTempOf(p string, s *outputSink) bool {
	if p == s.tempPath {
		return true
	}
	if s.sidecar != nil && p == s.sidecar.tmpPath {
		return true
	}
	return s.searchStaged != "" && p == s.searchStaged
}

func filterEvents(events []durableEvent, kind string) []durableEvent {
	var out []durableEvent
	for _, e := range events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func eventIndex(events []durableEvent, e durableEvent) int {
	return slices.Index(events, e)
}

// TestDurableSealSyncFailureKeepsOldOutput: an injected archive-temp fsync
// failure must fail seal (no success/history), never rename, and leave a
// pre-existing final output byte-for-byte intact.
func TestDurableSealSyncFailureKeepsOldOutput(t *testing.T) {
	d := t.TempDir()
	out := filepath.Join(d, "sfu_fail.txt.zst")
	sentinel := []byte("PREEXISTING OUTPUT\n")
	if werr := os.WriteFile(out, sentinel, 0o644); werr != nil {
		t.Fatal(werr)
	}
	sink, err := newOutputSinkWithSidecar(out, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if werr := sink.writeBatch([]byte("h.example.com:u:p\n"), 1, nil); werr != nil {
		t.Fatal(werr)
	}

	var events []durableEvent
	installDurableHooks(t, &events, func(path string) bool {
		return path == sink.tempPath
	}, nil)

	if serr := sink.seal(); serr == nil {
		t.Fatal("seal must fail when the archive temp fsync fails")
	}
	if aerr := sink.abort(); aerr != nil {
		t.Fatalf("abort: %v", aerr)
	}
	got, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("pre-existing output damaged: %q", got)
	}
	if exists(t, sink.tempPath) {
		t.Fatalf("staged temp %s survived abort", sink.tempPath)
	}
}

// TestDurableMultipartSyncsDirOnce: a multi-part commit must sync each
// affected directory exactly once after the batch, not once per part.
func TestDurableMultipartSyncsDirOnce(t *testing.T) {
	d := t.TempDir()
	stamp := RunStamp(time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC), "dur01a")
	c, err := newChunkedZstdSink(d, stamp, 2, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	var events []durableEvent
	installDurableHooks(t, &events, nil, nil)

	for i := range 6 {
		if werr := c.writeBatch([]byte(fmt.Sprintf("h%d.example.com:u:pw%d\n", i, i)), 1, nil); werr != nil {
			t.Fatal(werr)
		}
	}
	if serr := c.seal(); serr != nil {
		t.Fatal(serr)
	}
	if cerr := c.commit(); cerr != nil {
		t.Fatalf("commit: %v", cerr)
	}

	dirs := filterEvents(events, "sync-dir")
	// parts share one archive dir + one idx dir + one search-idx dir
	uniq := map[string]int{}
	for _, e := range dirs {
		uniq[e.path]++
	}
	for p, n := range uniq {
		if n != 1 {
			t.Errorf("dir %s synced %d times, want exactly once per multipart commit", p, n)
		}
	}
	wantDirs := map[string]bool{
		d:                               true,
		filepath.Join(d, idxSubdirName): true,
	}
	// search-idx subdir: derive from the first part's final search sidecar
	first := c.sealed[0].finalSearchSidecarPath()
	wantDirs[filepath.Dir(first)] = true
	if len(uniq) != len(wantDirs) {
		t.Errorf("synced dirs %v, want exactly %v", uniq, wantDirs)
	}
	for d := range wantDirs {
		if uniq[d] != 1 {
			t.Errorf("dir %s not synced exactly once", d)
		}
	}
}
