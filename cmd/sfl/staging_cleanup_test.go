package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// failWriteFile fails every write after a successful create, simulating a full
// disk mid-ingest without any permission trickery.
type failWriteFile struct{ *os.File }

var errInjectedWrite = errors.New("injected: no space left on device")

func (f *failWriteFile) Write(p []byte) (int, error) { return 0, errInjectedWrite }

// A writer error while draining the -od plaintext staging ULP must fail the run
// and leave no staging (or spill) directory behind: the failed output is torn
// down through the sink's existing cleanup lifecycle, never kept for the user
// to remove by hand.
func TestRunStagingDirRemovedOnSinkWriteError(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "tmp")
	lib := filepath.Join(dir, "lib")
	input := filepath.Join(dir, "in", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
		[]byte("URL: https://a.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := openStagingULP
	openStagingULP = func(path string) (io.WriteCloser, error) {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		return &failWriteFile{f}, nil
	}
	t.Cleanup(func() { openStagingULP = orig })

	err := run(runConfig{
		Input: input, LibraryDir: lib, TempDir: primary, Workers: 1, NoTUI: true,
		Started: time.Date(2026, 6, 26, 21, 2, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("expected the injected writer error to fail the run")
	}
	if !strings.Contains(err.Error(), errInjectedWrite.Error()) {
		t.Fatalf("run error %v must surface the injected writer error", err)
	}

	entries, rerr := os.ReadDir(primary)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "sfl-od-"):
			t.Errorf("plaintext staging dir left behind: %s", filepath.Join(primary, e.Name()))
		case strings.HasPrefix(e.Name(), "sfl-spill-"):
			t.Errorf("spill dir left behind: %s", filepath.Join(primary, e.Name()))
		}
	}
	// library dir may be pre-created by the orphan sweep, but a failed ingest
	// must never have written anything into it
	err = filepath.WalkDir(lib, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".zst") || strings.HasSuffix(p, ".idx")) {
			t.Errorf("library contains output after a failed ingest: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Happy-path counterpart: after a successful ingest the staging ULP is consumed
// and must be gone, and the registry must no longer hold it for force-exit
// cleanup (unregistered via sink.cleanup / the pipeline's success path).
func TestRunStagingDirRemovedAndUnregisteredAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "tmp")
	lib := filepath.Join(dir, "lib")
	input := filepath.Join(dir, "in", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"),
		[]byte("URL: https://a.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run(runConfig{
		Input: input, LibraryDir: lib, TempDir: primary, Workers: 1, NoTUI: true,
		Started: time.Date(2026, 6, 26, 21, 3, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(primary)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sfl-od-") {
			t.Errorf("staging dir left behind after success: %s", e.Name())
		}
	}
	// the ingested archive must exist...
	var foundArch bool
	filepath.WalkDir(lib, func(p string, d os.DirEntry, werr error) error {
		if werr == nil && !d.IsDir() && strings.HasSuffix(p, ".zst") {
			foundArch = true
		}
		return nil
	})
	if !foundArch {
		t.Fatal("no ingested .zst archive found in library")
	}
	// ...and nothing staging-related may linger in the cleanup registry
	for _, p := range ulpengine.SnapshotCleanupPaths() {
		base := filepath.Base(p)
		if strings.HasPrefix(base, "sfl-od-") || strings.HasPrefix(base, "sfl-spill-") {
			t.Errorf("staging path still registered after success: %s", p)
		}
	}
}

// sink.cleanup gates unregistration on confirmed absence: a staging dir whose
// removal fails (here: a non-directory parent component makes RemoveAll fail
// with ENOTDIR) must stay registered, while a removed one is unregistered.
// The registry is process-global and other staging tests in this package may
// hold their own entries, so assertions check membership, not exact equality;
// the test unregisters its own paths when done.
func TestSinkCleanupGatesUnregisterOnRemoval(t *testing.T) {
	base := t.TempDir()
	defer func() {
		ulpengine.UnregisterCleanupPath(filepath.Join(base, "blocker", "sfl-od-stuck"))
		ulpengine.UnregisterCleanupPath(filepath.Join(base, "gone"))
	}()

	// surviving case: blocker file makes the workDir path un-removable
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(blocker, "sfl-od-stuck")
	ulpengine.RegisterCleanupPath(stuck)
	s := &sink{workDir: stuck}
	s.cleanup() // RemoveTreeLogged fails (ENOTDIR), dir conceptually survives
	if !slices.Contains(ulpengine.SnapshotCleanupPaths(), stuck) {
		t.Fatal("surviving staging dir must stay registered")
	}

	// removed case: normal cleanup unregisters
	gone := filepath.Join(base, "gone")
	ulpengine.RegisterCleanupPath(gone)
	s2 := &sink{workDir: gone}
	s2.cleanup()
	if slices.Contains(ulpengine.SnapshotCleanupPaths(), gone) {
		t.Fatal("removed staging dir must be unregistered")
	}
}
