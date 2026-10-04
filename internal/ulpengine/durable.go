package ulpengine

import "os"

// Durability of the transactional output publish (P1-W1 seal/commit):
//
//	seal   — fsync the archive temp and every staged sidecar temp BEFORE
//	         close/rename, so renamed finals never expose unwritten bytes.
//	commit — rename archive + sidecars to their final names, then fsync each
//	         immediate parent directory after its final rename (multipart
//	         syncs each affected directory exactly once after the batch). A
//	         sync error fails the run BEFORE any history is recorded.
//
// Dry-run never creates a sink (discardSink), so it performs no fsync.
//
// The sync*Hook vars are test-only seams (nil in production): they inject
// failures and record ordered events for the durability tests. Windows has
// no portable directory FlushFileBuffers — durableSyncDirOS is a documented
// no-op there (see P5-W1); file syncs stay real on every platform.

var (
	durableSyncFileHook func(path string) error
	durableSyncDirHook  func(path string) error
	durableEventHook    func(kind, path string) // ordered event recorder
)

// syncFileDurably fsyncs a file's contents. The handle is opened O_RDWR so
// Windows' FlushFileBuffers (which requires write access) accepts it.
func syncFileDurably(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// durableSyncFile fsyncs one staged file (archive temp, .idx temp, search
// sidecar temp), recording the event for tests.
func durableSyncFile(path string) error {
	var err error
	if durableSyncFileHook != nil {
		err = durableSyncFileHook(path)
	} else {
		err = syncFileDurably(path)
	}
	if durableEventHook != nil {
		durableEventHook("sync-file", path)
	}
	return err
}

// durableSyncDir fsyncs one immediate parent directory whose entries a commit
// rename changed.
func durableSyncDir(path string) error {
	var err error
	if durableSyncDirHook != nil {
		err = durableSyncDirHook(path)
	} else {
		err = durableSyncDirOS(path)
	}
	if durableEventHook != nil {
		durableEventHook("sync-dir", path)
	}
	return err
}
