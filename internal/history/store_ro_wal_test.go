package history_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// H-11: mode=ro applies to the main database, but in WAL mode SQLite still
// opens/creates the -wal/-shm sidecars with write access — so consulting a
// cleanly-closed history database on read-only media failed with "attempt to
// write a readonly database". A quiescent read-only open (no sidecars on
// disk) must work from a non-writable parent directory and must not create
// sidecars.
func TestOpenReadOnlySidecarFreeDirectorySucceeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "hist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "history.sqlite3")

	store, err := history.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id := history.Identity{Hash: 0xdeadbeefcafe, Size: 42}
	if err := store.Record(context.Background(), []history.Identity{id}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A clean close checkpoints the WAL; sidecars may or may not linger
	// depending on the build. Remove them so the test pins the quiescent
	// read-only contract.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	roStore, exists, err := history.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly on read-only directory: %v", err)
	}
	if !exists {
		t.Fatal("OpenReadOnly reported exists=false for a present database")
	}
	defer func() {
		if cerr := roStore.Close(); cerr != nil {
			t.Errorf("close read-only store: %v", cerr)
		}
	}()
	got, err := roStore.Lookup(context.Background(), []history.Identity{id})
	if err != nil {
		t.Fatalf("lookup through read-only store: %v", err)
	}
	if _, ok := got[id]; !ok {
		t.Fatalf("recorded identity %v missing from read-only lookup: %v", id, got)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			t.Errorf("read-only open created sidecar %s; it must not mutate the parent", suffix)
		}
	}
}
