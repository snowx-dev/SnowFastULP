//go:build windows

package selfupdate

// Windows cannot overwrite a running executable, so replacing an existing
// binary takes the rename-aside dance (target → unique .old backup, then the
// staged .new → target). A crash between the renames would otherwise leave
// the executable missing with the backup deleted on the next blind retry.
// This file wires the swap to the recoverable journal in journal.go:
// the journal is published before the first destructive rename and
// recoverInterruptedUpdate repairs every later crash window.

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// activeReplaceJournal is the in-flight journaled replace for this process.
// The update lock guarantees a single active updater per install directory.
var activeReplaceJournal *replaceJournal

// beginReplaceJournal stages every existing-bin payload, sweeps leftovers of
// earlier update runs, and publishes the recovery journal atomically before
// the apply loop's first destructive rename. Returns a nil handle when there
// is nothing to journal (new installs only).
func beginReplaceJournal(dir string, pending []pendingUpdate, payloads [][]byte) (*replaceJournal, error) {
	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err != nil {
		activeReplaceJournal = nil
		return nil, err
	}
	activeReplaceJournal = rj
	return rj, nil
}

// replaceExistingBin swaps one existing binary through the journaled dance:
// the .new payload was staged and verified before the journal was published,
// so this only performs the renames and the verify+sync of the result.
func replaceExistingBin(_ []byte, target string, _ []byte) error {
	rj := activeReplaceJournal
	if rj == nil || rj.journal == nil {
		return fmt.Errorf("internal error: no active update journal while replacing %s", target)
	}
	e := rj.entry(target)
	if e == nil {
		return fmt.Errorf("internal error: target %s is not covered by the active update journal", target)
	}
	return swapTarget(e, rj.dir)
}

// hideFile marks a backup that could not be removed — the running
// executable's own image, which Windows refuses to delete — as hidden.
// The next update run's staging sweep
// removes it once the old process is long gone.
func hideFile(path string) error {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.SetFileAttributes(ptr, windows.FILE_ATTRIBUTE_HIDDEN)
}

// syncDir is a no-op on Windows. Update durability relies on NTFS metadata
// journaling; FAT/exFAT targets are not guaranteed crash-safe for selfupdate
// and are unsupported for the install directory. Staged .new payloads and the
// journal are flushed with file-level Sync before the journaled swap renames;
// the first-install (isNew) path renames an un-synced temp with no journal
// (the target did not previously exist).
func syncDir(dir string) error {
	_ = dir
	return nil
}
