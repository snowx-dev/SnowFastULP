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

func TestPrintManualCleanupHintFiltersMissing(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	alive := filepath.Join(dir, "alive")
	if err := os.WriteFile(alive, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dead := filepath.Join(dir, "dead-no-such-file")

	RegisterCleanupPath(alive)
	RegisterCleanupPath(dead)

	var buf bytes.Buffer
	PrintManualCleanupHint(&buf)
	out := buf.String()

	if !strings.Contains(out, alive) {
		t.Errorf("expected surviving path %q in hint, got: %q", alive, out)
	}
	if strings.Contains(out, dead) {
		t.Errorf("missing path %q should have been filtered, got: %q", dead, out)
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
	cleanupMu.Unlock()
}
