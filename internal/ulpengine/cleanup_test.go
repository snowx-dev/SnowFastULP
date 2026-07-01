package ulpengine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisterCleanupPathDedupe(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	RegisterCleanupPath(a)
	RegisterCleanupPath(b)
	RegisterCleanupPath(a)
	RegisterCleanupPath("")

	got := SnapshotCleanupPaths()
	want := []string{a, b}
	if len(got) != len(want) {
		t.Fatalf("snapshot len=%d, want %d (paths: %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestFlushRegisteredCleanupRemovesDirAndFile(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	tree := filepath.Join(dir, "scratch")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "partial.zst")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RegisterCleanupPath(tree)
	RegisterCleanupPath(file)

	FlushRegisteredCleanup()

	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree should be gone: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("file should be gone: %v", err)
	}
	log := SnapshotCleanupLog()
	if len(log) < 2 {
		t.Fatalf("expected cleanup log entries, got %v", log)
	}
}

func TestPrintManualCleanupHintFlushesFirst(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	tree := filepath.Join(dir, "orphan")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	RegisterCleanupPath(tree)

	var buf bytes.Buffer
	PrintManualCleanupHint(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected no hint after successful flush, got %q", buf.String())
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree should be flushed: %v", err)
	}
}

func TestPrintManualCleanupHintFiltersMissing(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dead := filepath.Join(t.TempDir(), "dead-no-such-file")
	RegisterCleanupPath(dead)

	var buf bytes.Buffer
	PrintManualCleanupHint(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output for missing-only registry, got %q", buf.String())
	}
}

func TestPrintManualCleanupHintEmptyIsNoOp(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	var buf bytes.Buffer
	PrintManualCleanupHint(&buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty registry, got %q", buf.String())
	}
}

// clears pkg-level cleanupPaths, test-only
func resetCleanupRegistry() {
	cleanupMu.Lock()
	cleanupPaths = nil
	cleanupLog = nil
	cleanupMu.Unlock()
}

func TestLogCleanupLineSnapshot(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	LogCleanupLine("removed /tmp/foo")
	LogCleanupLine("")
	LogCleanupLine("removed /tmp/bar")

	got := SnapshotCleanupLog()
	want := []string{"removed /tmp/foo", "removed /tmp/bar"}
	if len(got) != len(want) {
		t.Fatalf("snapshot len=%d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestRemovePathLogged(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	path := filepath.Join(dir, "partial.zst")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RemovePathLogged(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be gone: %v", err)
	}
	log := SnapshotCleanupLog()
	if len(log) != 1 || !strings.Contains(log[0], "removed") || !strings.Contains(log[0], "partial.zst") {
		t.Fatalf("unexpected log: %v", log)
	}
}
