package ulpengine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A successful -od run must leave nothing deletable in the registry: committed
// .zst/.idx/.sfsidx.json outputs are unregistered right before phaseDone, so a
// later force-exit flush spares them. The split rename (single part -> _part1)
// must also drop the stale pre-rename path.
func TestRunSuccessUnregistersCommittedOutputs(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	libDir := t.TempDir()
	in := filepath.Join(t.TempDir(), "in.txt")
	body := ""
	for i := 0; i < 5; i++ {
		body += fmt.Sprintf("https://%c.example.com:user%d:pw%d\n", 'a'+i, i+1, i+1)
	}
	if err := os.WriteFile(in, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	stamp := "regcln"
	output := filepath.Join(libDir, "sfu_"+stamp+".txt.zst")
	stage := filepath.Join(libDir, ".stage")
	r, err := Resolve(Config{
		Inputs:        []string{in},
		Output:        output,
		TempDir:       stage,
		FastPathOff:   true,
		Buckets:       4,
		Compress:      true,
		ZstChunkLines: 2, // 5 unique creds -> 3 parts -> forces the part-1 rename
		DestDedup:     true,
		DestDedupDir:  libDir,
		RunStamp:      stamp,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	m := &Metrics{}
	runRes := &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      stage,
	}
	if err := Run(context.Background(), runRes, m); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(runRes.OutputPaths) < 2 {
		t.Fatalf("expected split parts, got %v", runRes.OutputPaths)
	}
	if m.LinesUnique.Load() != 5 {
		t.Fatalf("linesUnique = %d, want 5", m.LinesUnique.Load())
	}

	// the pre-rename initial path is gone and unregistered
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("initial single-part path %s should have been renamed: %v", output, err)
	}

	var committed []string
	for _, p := range runRes.OutputPaths {
		committed = append(committed,
			p,
			sidecarPathForArchive(p))
	}
	// NOTE: the part-1 .sfsidx.json search sidecar is intentionally not asserted
	// here; its pre-rename name after the part-1 rotate is handled by the later
	// destination-sidecar step, not by cleanup registration.
	for _, p := range committed {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("committed output missing: %s: %v", p, err)
		}
	}

	registered := map[string]bool{}
	for _, p := range SnapshotCleanupPaths() {
		registered[p] = true
	}
	for _, p := range committed {
		if registered[p] {
			t.Errorf("committed output still registered: %s", p)
		}
	}
	if registered[output] {
		t.Error("stale pre-rename path still registered")
	}

	// flush must not touch the committed outputs
	FlushRegisteredCleanup()
	for _, p := range committed {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("committed output deleted by flush: %s: %v", p, err)
		}
	}
}

func TestSidecarWriterAbortUnregistersTempAndSpills(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()
	oldMax := sidecarSortMaxKeys
	sidecarSortMaxKeys = 2 // force spill run files
	t.Cleanup(func() { sidecarSortMaxKeys = oldMax })

	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_abort.txt.zst")
	sw, err := newSidecarWriter(archive)
	if err != nil {
		t.Fatalf("newSidecarWriter: %v", err)
	}
	for k := uint64(1); k <= 5; k++ {
		if err := sw.WriteHash(k); err != nil {
			t.Fatalf("WriteHash: %v", err)
		}
	}
	if len(sw.spills) == 0 {
		t.Fatal("expected spill run files")
	}
	if got := SnapshotCleanupPaths(); len(got) < 2 {
		t.Fatalf("temp+spills should be registered during the write, got %v", got)
	}
	if err := sw.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	if got := SnapshotCleanupPaths(); len(got) != 0 {
		t.Fatalf("removed temp/spills must be unregistered, got %v", got)
	}
	// an empty registry flush cannot resurrect or delete anything
	FlushRegisteredCleanup()
	if _, err := os.Stat(sidecarPathForArchive(archive)); !os.IsNotExist(err) {
		t.Fatalf("aborted sidecar must stay absent: %v", err)
	}
}

func TestSidecarWriterCommitUnregistersTempKeepsFinal(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()
	oldMax := sidecarSortMaxKeys
	sidecarSortMaxKeys = 2 // force spill run files
	t.Cleanup(func() { sidecarSortMaxKeys = oldMax })

	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_commit.txt.zst")
	// v4 sidecars bind to the archive's identity at Commit: create it
	if err := os.WriteFile(archive, []byte("archive payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	sw, err := newSidecarWriter(archive)
	if err != nil {
		t.Fatalf("newSidecarWriter: %v", err)
	}
	for k := uint64(1); k <= 5; k++ {
		if err := sw.WriteHash(k); err != nil {
			t.Fatalf("WriteHash: %v", err)
		}
	}
	if _, err := sw.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := SnapshotCleanupPaths(); len(got) != 0 {
		t.Fatalf("committed temp+spills must be unregistered, got %v", got)
	}
	final := sidecarPathForArchive(archive)
	FlushRegisteredCleanup()
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("committed .idx must survive flush: %v", err)
	}
}

// When Abort cannot actually remove the temp path (here: a non-empty directory
// stands in for a stuck file), the path must stay registered so a force-exit
// can still clean it up; unregistration happens only once it is really gone.
func TestSidecarWriterAbortRetainsRegistrationWhenRemovalFails(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	stubborn := filepath.Join(dir, "stubborn.write.tmp")
	if err := os.Mkdir(stubborn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubborn, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RegisterCleanupPath(stubborn)
	w := &sidecarWriter{tmpPath: stubborn}

	if err := w.Abort(); err == nil {
		t.Fatal("expected Abort to report the failed removal")
	}
	if got := SnapshotCleanupPaths(); len(got) != 1 || got[0] != stubborn {
		t.Fatalf("registration must be retained while the path survives, got %v", got)
	}

	// once the path becomes removable, a later Abort unregisters it
	if err := os.Remove(filepath.Join(stubborn, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if got := SnapshotCleanupPaths(); len(got) != 0 {
		t.Fatalf("unregistration expected once the path is gone, got %v", got)
	}
}

// A commit whose rename fails (target is a directory) removes the temp file and
// unregisters it — the temp is gone, so it is no longer a deletable artifact.
func TestSidecarWriterCommitRenameFailureUnregistersTemp(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	// an existing directory as the rename target makes atomicfs.Rename fail
	if err := os.Mkdir(filepath.Join(dir, "sfu_renamefail.txt.zst.idx"), 0o755); err != nil {
		t.Fatal(err)
	}
	// identity source is irrelevant: finish() fails before any rename here
	w, err := newSidecarWriterAtPath(filepath.Join(dir, "sfu_renamefail.txt.zst.idx"), filepath.Join(dir, "sfu_renamefail.txt.zst"))
	if err != nil {
		t.Fatalf("newSidecarWriterAtPath: %v", err)
	}
	if got := SnapshotCleanupPaths(); len(got) != 1 {
		t.Fatalf("temp should be registered during the write, got %v", got)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("expected Commit to fail on the blocked rename target")
	}

	if got := SnapshotCleanupPaths(); len(got) != 0 {
		t.Fatalf("removed temp must be unregistered, got %v", got)
	}
	FlushRegisteredCleanup()
	// the failed sidecar must not have been written anywhere
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".write.*.tmp") || (!e.IsDir() && strings.HasSuffix(e.Name(), ".tmp")) {
			t.Errorf("temp file left behind after failed commit: %s", e.Name())
		}
	}
}
