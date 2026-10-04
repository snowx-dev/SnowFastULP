//go:build windows

package durablefs

import (
	"path/filepath"
	"testing"
)

// Portable directory FlushFileBuffers is not supported reliably on Windows, so
// syncDirectory must be a no-op that never opens anything: a drive root,
// network share, or read-only ancestor can never fail an otherwise completed
// output this way.
func TestSyncDirectoryIsNoOp(t *testing.T) {
	if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("syncDirectory(missing) = %v; want a no-op that never opens the path", err)
	}
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatalf("syncDirectory(existing) = %v; want a no-op", err)
	}
}
