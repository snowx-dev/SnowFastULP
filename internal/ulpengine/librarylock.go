package ulpengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Advisory per-library serialization lock. Phase 0 (-od) mutates the library
// (orphan sweep, sidecar regen, v2→v3 upgrade) and the final output commit
// appends a new archive+sidecar pair; two overlapping sfu runs on the same
// destination directory must never run those concurrently or one run's
// discovery can see a half-committed state and its sweeps can race the other
// run's writes. The lock is acquired before runODScan starts and held until
// the run's outputs are committed (or the run fails), so runs to one library
// execute their phase-0-through-commit spans strictly one at a time.
//
// The lock is an exclusive-or-shared advisory lock on a small lock file inside
// the library directory itself:
//   - real runs take EXCLUSIVE (they mutate the library and commit outputs)
//   - dry-run (-odr) takes SHARED (it only reads a coherent snapshot), so
//     multiple dry-runs overlap while still serializing against real runs
//   - Unix: flock(2), Windows: LockFileEx — both advisory, both released
//     automatically when the process dies, so a crashed run cannot deadlock
//     the next one.
//   - dry-run never creates the lock file (a preview must not mutate the
//     library, not even a hidden dotfile); it locks only if the file already
//     exists, and proceeds unlocked with a debug note if it cannot.
//
// No equivalent repository lock exists; the lock covers exactly one library
// directory. Sibling libraries use independent lock files and do not block
// each other.

// errLibraryLockUnsupported aborts acquire immediately on platforms with no
// advisory lock primitive (declared here; returned by the !unix,!windows stub).
var errLibraryLockUnsupported = errors.New("library lock: no advisory lock implementation for this platform")

// libraryLockFileName is a hidden file so it does not look like library
// content and is skipped by the sfu_*.txt.zst discovery glob.
const libraryLockFileName = ".sfu-library.lock"

func libraryLockPath(destDir string) string {
	return filepath.Join(destDir, libraryLockFileName)
}

// libraryLock holds the platform lock handle. release is safe to call on a
// nil lock and multiple times; release must be called from the same process
// (the OS drops the lock at process exit as a last-resort safety net).
type libraryLock struct {
	unlocked func() error // platform unlock+close, idempotent
	released atomic.Bool
	dbg      *DebugLog // optional, for acquire/release events
	dest     string
}

// acquireLibraryLock takes the advisory lock on destDir, retrying
// (non-blocking attempt + short sleep) until it succeeds or ctx is done.
// shared=true takes a shared lock (dry-run), otherwise exclusive. When
// canCreate is false the lock file is not created: an absent file skips
// locking entirely (nil, nil) instead of mutating the directory.
func acquireLibraryLock(ctx context.Context, destDir string, shared, canCreate bool, dbg *DebugLog) (*libraryLock, error) {
	if destDir == "" {
		return nil, fmt.Errorf("library lock: dest dir is empty")
	}
	f, err := openLibraryLockFile(libraryLockPath(destDir), canCreate)
	if err != nil {
		return nil, err
	}
	if f == nil {
		// dry-run on a library no real run has touched yet: nothing to
		// serialize against, and the preview must not create files.
		return nil, nil
	}
	mode := lockModeExclusive
	if shared {
		mode = lockModeShared
	}
	backoff := 10 * time.Millisecond
	for {
		err := tryLockLibraryFile(f, mode)
		if err == nil {
			if dbg != nil {
				dbg.Event("[od] library lock acquired (%s) dest=%s", lockModeName(mode), destDir)
			}
			return &libraryLock{
				unlocked: func() error {
					uerr := unlockLibraryFile(f, mode)
					if cerr := f.Close(); uerr == nil {
						uerr = cerr
					}
					return uerr
				},
				dbg:  dbg,
				dest: destDir,
			}, nil
		}
		if errors.Is(err, errLibraryLockUnsupported) {
			_ = f.Close()
			return nil, err
		}
		if dbg != nil {
			dbg.Event("[od] library lock busy (%s), retrying: %v", lockModeName(mode), err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("library lock %s: %w", destDir, ctx.Err())
		case <-time.After(backoff):
		}
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
}

// release unlocks and closes the lock file. Idempotent; nil-safe.
func (l *libraryLock) release() {
	if l == nil || !l.released.CompareAndSwap(false, true) {
		return
	}
	if l.dbg != nil {
		l.dbg.Event("[od] library lock released dest=%s", l.dest)
	}
	_ = l.unlocked()
}

func lockModeName(shared lockMode) string {
	if shared == lockModeShared {
		return "shared"
	}
	return "exclusive"
}

type lockMode int

const (
	lockModeExclusive lockMode = iota
	lockModeShared
)

// openLibraryLockFile opens (creating when allowed) the lock file. Returns
// (nil, nil) when canCreate is false and the file does not exist. There is no
// check-then-create TOCTOU: canCreate opens are a single atomic
// create-or-open syscall, so a second opener shares the same inode and the
// advisory lock still serializes — O_EXCL would add nothing.
//
// Residual first-use window (accepted, do not "fix" with a create): a dry-run
// (canCreate=false) that opens just before the first real run creates the
// lock file gets nil, nil and previews unlocked. Worst case it reads the
// library mid-mutation of that real run — wrong preview counts, never
// corruption, because the preview writes nothing. Closing the window would
// require the dry-run to create+remove a lock file, which mutates the library
// (breaking the preview-must-not-write invariant) and the unlink-while-held
// inode swap would defeat the lock entirely.
func openLibraryLockFile(path string, canCreate bool) (*os.File, error) {
	if canCreate {
		return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}
