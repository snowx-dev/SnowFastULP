//go:build !windows

package selfupdate

// Non-Windows platforms can overwrite a binary in place atomically
// (rename(2) atomically replaces the destination), so existing-bin
// replacement is a single local dance: write the payload to a hidden temp
// file in the target's directory, fsync it, chmod it to the target's mode,
// then one rename(2) over the target. The target pathname is never absent —
// unlike the old target→.target.old, .new→target two-rename dance, there is
// no crash or power-loss window in which the executable is missing, so no
// journal is needed. The containing directory is fsynced to make the rename
// durable. recoverInterruptedUpdate remains a no-op here.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// beginReplaceJournal is a no-op on non-Windows platforms.
func beginReplaceJournal(_ string, _ []pendingUpdate, _ [][]byte) (*replaceJournal, error) {
	return nil, nil
}

// replaceExistingBin atomically replaces an existing binary: the payload was
// checksum-verified by the caller (applyPayloadFor), so this only stages it
// durably and performs the single atomic rename over the target.
func replaceExistingBin(data []byte, target string, _ []byte) error {
	dir := filepath.Dir(target)
	fi, err := os.Stat(target)
	mode := os.FileMode(0o755)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat target %s: %w", target, err)
		}
	} else {
		mode = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".replace-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write payload: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("swap binary into place: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

// hideFile is unreachable in production on non-Windows platforms (only the
// journaled replace keeps a removable-then-hide backup), but a backup that
// cannot be removed must not be silently discarded either: fail loudly.
func hideFile(path string) error {
	return fmt.Errorf("hiding %s is not supported on this platform", path)
}

// syncDir makes a directory-entry change durable on Unix.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s for sync: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return d.Close()
}
