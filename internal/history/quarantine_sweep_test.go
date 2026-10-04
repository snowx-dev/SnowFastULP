package history_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/history"
)

// staleContainerName returns a syntactically valid quarantine container name
// as one left behind by a crashed process: ".snowfast-quarantine-" + 32 hex.
func staleContainerName() string {
	return ".snowfast-quarantine-0123456789abcdef0123456789abcdef"
}

// backdateContainer ages a legacy (metadata-less) container past the sweep
// lease so a crashed run simulated in a test is recoverable immediately.
func backdateContainer(t *testing.T, container string) {
	t.Helper()
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(container, past, past); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteStagedRestoresStaleQuarantinePayload(t *testing.T) {
	parent := t.TempDir()
	// Simulate a crashed run: the source was staged (renamed into a
	// container) but never validated or removed.
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	backdateContainer(t, container)
	removed, err := history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil)
	if err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if len(removed) != 1 || removed[0] != livePath {
		t.Fatalf("removed = %v, want [%s]", removed, livePath)
	}

	restored, readErr := os.ReadFile(filepath.Join(parent, "orphan.txt"))
	if readErr != nil {
		t.Fatalf("stale quarantine payload was not restored to its original path: %v", readErr)
	}
	if string(restored) != "staged" {
		t.Fatalf("restored content = %q, want %q", restored, "staged")
	}
	if _, statErr := os.Lstat(container); !os.IsNotExist(statErr) {
		t.Fatalf("restored container was not removed: %v", statErr)
	}
	if _, statErr := os.Lstat(livePath); !os.IsNotExist(statErr) {
		t.Fatalf("live source was not deleted: %v", statErr)
	}
}

func TestDeleteStagedRemovesEmptyStaleQuarantineContainer(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	backdateContainer(t, container)
	if _, err = history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Lstat(container); !os.IsNotExist(statErr) {
		t.Fatalf("empty stale container was not removed: %v", statErr)
	}
}

func TestDeleteStagedRestoresStaleQuarantineDirectoryPayload(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	stagedDir := filepath.Join(container, "orphan-dir")
	if err := os.Mkdir(stagedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagedDir, "member.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	backdateContainer(t, container)
	if _, err = history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(parent, "orphan-dir", "member.txt")); statErr != nil {
		t.Fatalf("stale quarantine directory payload was not restored: %v", statErr)
	}
	if _, statErr := os.Lstat(container); !os.IsNotExist(statErr) {
		t.Fatalf("restored container was not removed: %v", statErr)
	}
}

func TestDeleteStagedLeavesStalePayloadWhenOriginalPathOccupied(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "orphan.txt"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A new file now occupies the original path; the sweep must not replace it.
	if err := os.WriteFile(filepath.Join(parent, "orphan.txt"), []byte("late arrival"), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	backdateContainer(t, container)
	if _, err = history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	got, readErr := os.ReadFile(filepath.Join(parent, "orphan.txt"))
	if readErr != nil || string(got) != "late arrival" {
		t.Fatalf("late original replacement was clobbered: %q, %v", got, readErr)
	}
	if _, statErr := os.Lstat(container); statErr != nil {
		t.Fatalf("stale container holding unrestorable payload was removed: %v", statErr)
	}
	kept, readErr := os.ReadFile(filepath.Join(container, "orphan.txt"))
	if readErr != nil || string(kept) != "staged" {
		t.Fatalf("staged data was lost: %q, %v", kept, readErr)
	}
}

func TestDeleteStagedLeavesStaleContainerWithUnexpectedContents(t *testing.T) {
	parent := t.TempDir()
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(container, "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	backdateContainer(t, container)
	if _, err = history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Lstat(container); statErr != nil {
		t.Fatalf("container with unexpected contents was modified: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(container, "a.txt")); statErr != nil {
		t.Fatalf("unexpected container contents were modified: %v", statErr)
	}
}

func TestDeleteStagedIgnoresSymlinkedStaleContainer(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, staleContainerName())
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Fatalf("symlinked container name was followed or removed: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(outside)); statErr != nil {
		t.Fatalf("symlink target was modified: %v", statErr)
	}
}

func TestDeleteStagedIgnoresSymlinkEntryInStaleContainer(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(parent, staleContainerName())
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(container, "orphan.txt")); err != nil {
		t.Fatal(err)
	}
	livePath := filepath.Join(parent, "live.txt")
	if err := os.WriteFile(livePath, []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), livePath, nil)
	if err != nil {
		t.Fatal(err)
	}

	backdateContainer(t, container)
	if _, err = history.DeleteStaged(context.Background(), []string{livePath}, []history.Candidate{candidate}, nil); err != nil {
		t.Fatalf("DeleteStaged: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(container, "orphan.txt")); statErr != nil {
		t.Fatalf("symlink entry was followed or removed: %v", statErr)
	}
}

// A crashed run must be recoverable: the staged payload keeps its original
// basename so a later sweep can rename it back without any side-channel.
func TestStagedPayloadKeepsOriginalBasename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := history.FingerprintFile(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if _, err = history.DeleteStaged(context.Background(), []string{path}, []history.Candidate{candidate}, func(staged []history.StagedPath) error {
		got = filepath.Base(staged[0].Payload)
		return errors.New("stop before deletion")
	}); err == nil {
		t.Fatal("expected afterStage error to propagate")
	}
	if got != "source.bin" {
		t.Fatalf("staged payload basename = %q, want %q", got, "source.bin")
	}
}
