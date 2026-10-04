//go:build windows

package ulpengine

// durableSyncDirOS is a documented no-op on Windows: there is no portable
// FlushFileBuffers for directory handles, and forcing one fails with
// ACCESS_DENIED (same disposition as internal/durablefs, P5-W1). File syncs
// remain real; directory-entry durability rides the platform's rename
// metadata semantics.
func durableSyncDirOS(path string) error { return nil }
