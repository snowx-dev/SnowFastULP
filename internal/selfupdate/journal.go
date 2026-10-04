package selfupdate

// Recoverable self-update journal (RR-1.10).
//
// On Windows an existing binary cannot be overwritten in place: the running
// process holds the image, so the swap is a rename-aside dance: target →
// .target.old, then .target.new → target. Power loss or a hard
// termination between those two renames leaves the executable MISSING, and
// blindly retrying is unsafe because the next swap deletes any existing
// .old backup before swapping — potentially destroying the only
// remaining copy of the binary.
//
// This file implements the repair: a version-1 JSON journal per install
// directory, written atomically (temp + rename + directory sync) BEFORE the
// first destructive rename, listing for every target the unique same-directory
// .new payload path, the unique .old backup path, the expected SHA-256 of the
// new binary, the SHA-256 of the old binary, and a state. Before any new
// update plan is applied, recoverInterruptedUpdate repairs every journal
// state a crash can produce from on-disk evidence alone:
//
//   - target missing, backup present  → restore backup, drop staged .new
//   - target hashes to the new SHA-256 → swap completed: drop backup/.new
//   - target hashes to the old SHA-256, no backup → aborted before any
//     rename: drop staged .new
//   - anything else (target bytes match neither hash, backup bytes tampered,
//     corrupt JSON, unknown version) → fail loudly naming every path while
//     preserving all files; nothing is deleted.
//
// The journal's state field is deliberately never rewritten after publish:
// a crash between a rename and a hypothetical state rewrite would leave the
// field lagging the filesystem, so recovery MUST stay evidence-driven anyway,
// and a single publish keeps the crash windows down to the rename boundaries
// the recovery cases above already cover.

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// journalFileName is the per-install-directory journal, a sibling of the
	// update lock (see lockFileName).
	journalFileName = ".snowfast-update.journal"

	// journalTempPrefix backs the atomic publish: temp + fsync + rename.
	journalTempPrefix = journalFileName + ".tmp-"

	// journalVersion is the only journal format this updater understands.
	// A journal written by a different version fails recovery loudly (files
	// preserved) instead of guessing at an incompatible layout.
	journalVersion = 1

	// journalStatePending is the only state the journal is ever written
	// with: it means "published, renames not yet started". Recovery is
	// evidence-driven (target presence + content hashes), so no further
	// states are needed; unknown states are rejected as corrupt.
	journalStatePending = "pending"

	// hashBufSize bounds the scratch buffer used when hashing install
	// payloads (release binaries are ~5 MiB today, capped at maxDownloadSize).
	hashBufSize = 1 << 20
)

// journalPath returns the journal file path for an install directory.
func journalPath(dir string) string {
	return filepath.Join(dir, journalFileName)
}

// journalEntry describes one existing binary being replaced under journal.
type journalEntry struct {
	Target     string `json:"target"`
	NewPath    string `json:"new_path"`
	BackupPath string `json:"backup_path"`
	NewSHA256  string `json:"new_sha256"`
	OldSHA256  string `json:"old_sha256"`
	State      string `json:"state"`
}

// updateJournal is the version-1 on-disk journal document.
type updateJournal struct {
	Version int            `json:"version"`
	Entries []journalEntry `json:"entries"`
}

// knownJournalStates is the set of state values recovery accepts; an unknown
// value means the journal was not written by this updater and is corrupt.
var knownJournalStates = map[string]bool{
	journalStatePending: true,
}

func isHexSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// validate checks one entry against the journal directory. Recovery only
// touches paths inside the journal's own directory, so a hand-crafted or
// truncated journal can never point the repair at unrelated files.
func (e *journalEntry) validate(dir string) error {
	cleanDir := filepath.Clean(dir)
	paths := map[string]string{
		"target": e.Target, "new_path": e.NewPath, "backup_path": e.BackupPath,
	}
	for name, p := range paths {
		if p == "" {
			return fmt.Errorf("journal entry %s is empty", name)
		}
		if !filepath.IsAbs(p) {
			return fmt.Errorf("journal entry %s %q is not absolute", name, p)
		}
		if filepath.Clean(p) != p {
			return fmt.Errorf("journal entry %s %q is not a clean path", name, p)
		}
		if !sameJournalDir(cleanDir, filepath.Dir(p)) {
			return fmt.Errorf("journal entry %s %q is outside the journal directory %s", name, p, cleanDir)
		}
	}
	if e.Target == e.NewPath || e.Target == e.BackupPath || e.NewPath == e.BackupPath {
		return fmt.Errorf("journal entry paths must be distinct: %s, %s, %s", e.Target, e.NewPath, e.BackupPath)
	}
	if !isHexSHA256(e.NewSHA256) {
		return fmt.Errorf("journal entry new_sha256 %q is not a SHA-256 hex digest", e.NewSHA256)
	}
	if !isHexSHA256(e.OldSHA256) {
		return fmt.Errorf("journal entry old_sha256 %q is not a SHA-256 hex digest", e.OldSHA256)
	}
	if !knownJournalStates[e.State] {
		return fmt.Errorf("journal entry state %q is unknown", e.State)
	}
	return nil
}

func (j *updateJournal) validate(dir string) error {
	if j.Version != journalVersion {
		return fmt.Errorf("journal version %d is unsupported (want %d)", j.Version, journalVersion)
	}
	if len(j.Entries) == 0 {
		return fmt.Errorf("journal has no entries")
	}
	for i := range j.Entries {
		if err := (&j.Entries[i]).validate(dir); err != nil {
			return fmt.Errorf("journal entry %d: %w", i, err)
		}
	}
	return nil
}

// parseJournal decodes and validates journal content. path is only used for
// error messages.
func parseJournal(data []byte, path string) (*updateJournal, error) {
	var j updateJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("journal %s is corrupt (%v) — no files were modified; resolve the install directory manually before updating", path, err)
	}
	if err := j.validate(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("journal %s is corrupt: %w — no files were modified; resolve the install directory manually before updating", path, err)
	}
	return &j, nil
}

// writeJournalAtomically publishes the journal with temp + fsync + rename so
// a reader never observes a partial journal and the content survives a crash
// before the first destructive rename. The directory-entry rename itself is
// made durable by syncDir.
func writeJournalAtomically(dir string, j *updateJournal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, journalTempPrefix)
	if err != nil {
		return fmt.Errorf("create journal temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	keepTemp := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return keepTemp(fmt.Errorf("write journal: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return keepTemp(fmt.Errorf("sync journal: %w", err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close journal temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod journal temp: %w", err)
	}
	if err := os.Rename(tmpName, journalPath(dir)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("publish journal %s: %w", journalPath(dir), err)
	}
	return syncDir(dir)
}

// removeJournalFile removes the journal once recovery/apply finished cleanly.
func removeJournalFile(dir string) error {
	if err := os.Remove(journalPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove journal %s: %w", journalPath(dir), err)
	}
	return syncDir(dir)
}

// journalFaultPoint names a crash window in the journaled replace. A fault
// hook (test seam) returning an error at one of these points simulates a hard
// crash: the caller aborts immediately WITHOUT cleanup, leaving the journal
// and staged files exactly as a power loss would.
type journalFaultPoint string

const (
	faultAfterJournalPublish journalFaultPoint = "after-journal-publish"
	faultAfterTargetBackup   journalFaultPoint = "after-target-backup"
	faultAfterNewTarget      journalFaultPoint = "after-new-target"
)

// journalFaultHook, when non-nil, fires at the named crash points of the
// journaled replace; a non-nil return simulates a crash (see journalFaultPoint).
var journalFaultHook func(point journalFaultPoint, e *journalEntry) error

func journalFault(point journalFaultPoint, e *journalEntry) error {
	if journalFaultHook == nil {
		return nil
	}
	return journalFaultHook(point, e)
}

// replaceJournal is the handle for one run's journaled replacement of
// existing binaries. nil is a valid no-op handle (non-Windows platforms
// replace via a single atomic rename and never journal).
type replaceJournal struct {
	dir     string
	journal *updateJournal
}

// entry returns the journal entry replacing target, or nil.
func (rj *replaceJournal) entry(target string) *journalEntry {
	if rj == nil || rj.journal == nil {
		return nil
	}
	for i := range rj.journal.Entries {
		if rj.journal.Entries[i].Target == target {
			return &rj.journal.Entries[i]
		}
	}
	return nil
}

// finish drops the journal after every journaled target was replaced and
// verified. Safe on a nil handle.
func (rj *replaceJournal) finish() error {
	if rj == nil {
		return nil
	}
	return removeJournalFile(rj.dir)
}

// uniqueJournalPath allocates a fresh same-directory sibling name
// ".<base>.<kind>-<random>" that does not currently exist, so a rename onto
// it can never clobber an unrelated file.
func uniqueJournalPath(dir, base, kind string) (string, error) {
	for attempt := 0; attempt < 64; attempt++ {
		var rnd [8]byte
		if _, err := crand.Read(rnd[:]); err != nil {
			return "", fmt.Errorf("generate swap path: %w", err)
		}
		p := filepath.Join(dir, fmt.Sprintf(".%s.%s-%s", base, kind, hex.EncodeToString(rnd[:])))
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
	}
	return "", fmt.Errorf("could not allocate a unique .%s path for %s in %s", kind, base, dir)
}

// stageNewBinary writes the verified payload to its unique .new path with
// O_EXCL, fsyncs the content, and closes the handle (Windows refuses to
// rename a file that is still open).
func stageNewBinary(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return withPermHint(fmt.Errorf("stage new binary %s: %w", path, err))
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write staged binary %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("sync staged binary %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close staged binary %s: %w", path, err)
	}
	return nil
}

// fileSHA256Hex hashes a file, returning lowercase hex. A missing file is
// reported via fs.ErrNotExist so callers can branch on it.
func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, hashBufSize)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// syncFile flushes an already-closed file's content to storage (the handle
// is reopened read/write, which Windows FlushFileBuffers requires).
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", path, err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("sync %s: %w", path, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close synced %s: %w", path, closeErr)
	}
	return nil
}

// removeStaleSwapArtifacts best-effort deletes leftovers of earlier update
// runs for base: a .old backup that could not be removed because it was the
// running executable's image (hidden instead), a
// .new staged before a pre-publish crash, or an unpublished journal temp.
// Called with the update lock held, so nothing live can be referencing them.
func removeStaleSwapArtifacts(dir, base string) {
	patterns := []string{
		filepath.Join(dir, "."+base+".new-*"),
		filepath.Join(dir, "."+base+".old-*"),
		filepath.Join(dir, journalTempPrefix+"*"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

// stageReplaceJournal stages every existing-bin payload and publishes the
// journal atomically before any destructive rename. It returns nil when
// there is nothing to journal (new installs only). On any failure the
// already-staged .new files are removed and nothing destructive has happened.
func stageReplaceJournal(dir string, pending []pendingUpdate, payloads [][]byte) (*replaceJournal, error) {
	j := &updateJournal{Version: journalVersion}
	staged := make([]string, 0, len(pending))
	cleanup := func() {
		for _, p := range staged {
			os.Remove(p)
		}
	}
	for i, u := range pending {
		if u.isNew {
			continue // nothing to swap; a crash during a new install cannot lose data
		}
		base := filepath.Base(u.target)
		removeStaleSwapArtifacts(dir, base)
		oldHash, err := fileSHA256Hex(u.target)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("hash existing %s before journaled replace: %w", u.target, err)
		}
		newPath, err := uniqueJournalPath(dir, base, "new")
		if err != nil {
			cleanup()
			return nil, err
		}
		backupPath, err := uniqueJournalPath(dir, base, "old")
		if err != nil {
			cleanup()
			return nil, err
		}
		if err := stageNewBinary(newPath, payloads[i]); err != nil {
			cleanup()
			return nil, err
		}
		staged = append(staged, newPath)
		j.Entries = append(j.Entries, journalEntry{
			Target:     u.target,
			NewPath:    newPath,
			BackupPath: backupPath,
			NewSHA256:  hex.EncodeToString(u.hash),
			OldSHA256:  oldHash,
			State:      journalStatePending,
		})
	}
	if len(j.Entries) == 0 {
		return nil, nil
	}
	// The journal write precedes the first destructive rename: from here on
	// a crash in every later window is recoverable via the on-disk evidence.
	if err := writeJournalAtomically(dir, j); err != nil {
		cleanup()
		return nil, err
	}
	if err := journalFault(faultAfterJournalPublish, &j.Entries[0]); err != nil {
		// Simulated crash: leave journal and staged files for recovery.
		return nil, fmt.Errorf("simulated crash after journal publish: %w", err)
	}
	return &replaceJournal{dir: dir, journal: j}, nil
}

// swapTarget performs the journaled replacement of one existing binary.
// Precondition: the journal is published and e.NewPath holds the verified
// payload. Every step after the first rename is recoverable by
// recoverInterruptedUpdate, so a failure here never attempts ad-hoc repair:
// the caller runs recovery (live failure or next start alike).
func swapTarget(e *journalEntry, dir string) error {
	// target → backup. After this rename the target is missing but the
	// backup holds the only good copy — the journal already names both.
	if err := os.Rename(e.Target, e.BackupPath); err != nil {
		return fmt.Errorf("rename %s → backup %s: %w", e.Target, e.BackupPath, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync directory after backing up %s: %w", e.Target, err)
	}
	if err := journalFault(faultAfterTargetBackup, e); err != nil {
		return fmt.Errorf("simulated crash after target→backup: %w", err)
	}
	// new → target. On failure the backup is untouched; recovery restores it.
	if err := os.Rename(e.NewPath, e.Target); err != nil {
		return fmt.Errorf("rename staged %s → %s (backup %s is preserved for recovery): %w", e.NewPath, e.Target, e.BackupPath, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync directory after installing %s: %w", e.Target, err)
	}
	if err := journalFault(faultAfterNewTarget, e); err != nil {
		return fmt.Errorf("simulated crash after new→target: %w", err)
	}
	// verify + sync target
	got, err := fileSHA256Hex(e.Target)
	if err != nil {
		return fmt.Errorf("verify replaced %s (backup %s preserved for recovery): %w", e.Target, e.BackupPath, err)
	}
	if got != strings.ToLower(e.NewSHA256) {
		return fmt.Errorf("replaced %s hashes to %s, want %s — the update journal stays in place and the backup %s is preserved; the next update run will attempt repair",
			e.Target, got, e.NewSHA256, e.BackupPath)
	}
	if err := syncFile(e.Target); err != nil {
		return fmt.Errorf("sync replaced %s: %w", e.Target, err)
	}
	// remove backup. A locked backup (the running executable's image on
	// Windows) is hidden instead of failing the update — the same tradeoff
	// the rename-aside dance makes — and the next run's staging sweep
	// removes it.
	if err := os.Remove(e.BackupPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if herr := hideFile(e.BackupPath); herr != nil {
			return fmt.Errorf("update of %s succeeded but the backup %s could not be removed or hidden: %v", e.Target, e.BackupPath, herr)
		}
	}
	return nil
}

// errJournalAmbiguous marks a journal whose on-disk state cannot be repaired
// automatically. Every referenced path is preserved; the message names them
// so the operator can resolve the directory manually.
type errJournalAmbiguous struct {
	journal string
	entry   journalEntry
	detail  string
}

func (e *errJournalAmbiguous) Error() string {
	return fmt.Sprintf(
		"interrupted self-update cannot be recovered safely (%s); no files were modified — inspect the target %s, the backup %s and the staged payload %s, then delete the journal %s once the install directory holds the binaries you want",
		e.detail, e.entry.Target, e.entry.BackupPath, e.entry.NewPath, e.journal)
}

// recoverInterruptedUpdate repairs an interrupted journaled update in dir.
// It is called at updater startup after the update lock is acquired and
// before any target inspection, and on the live failure path of a run. With
// no journal present it is a no-op (non-Windows platforms never write one).
// Every recovery decision is made from on-disk evidence; ambiguous or corrupt
// journals fail loudly and preserve every file.
func recoverInterruptedUpdate(dir string) error {
	path := journalPath(dir)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read self-update journal %s: %w — refusing to plan an update until it is resolved", path, err)
	}
	j, err := parseJournal(data, path)
	if err != nil {
		return err
	}
	for i := range j.Entries {
		if err := recoverJournalEntry(&j.Entries[i], dir, path); err != nil {
			return err
		}
	}
	return removeJournalFile(dir)
}

// recoverJournalEntry repairs one entry from on-disk evidence. On any
// ambiguity it returns without deleting anything.
func recoverJournalEntry(e *journalEntry, dir, journalFile string) error {
	targetHash, targetErr := fileSHA256Hex(e.Target)
	switch {
	case errors.Is(targetErr, fs.ErrNotExist):
		return recoverMissingTarget(e, dir, journalFile)
	case targetErr != nil:
		return fmt.Errorf("hash journal target %s (no recovery performed): %w", e.Target, targetErr)
	}
	if targetHash == strings.ToLower(e.NewSHA256) {
		// The swap completed; finish its cleanup.
		if err := os.Remove(e.BackupPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove leftover backup %s for completed update of %s: %w", e.BackupPath, e.Target, err)
		}
		if err := os.Remove(e.NewPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove leftover staged file %s for completed update of %s: %w", e.NewPath, e.Target, err)
		}
		return nil
	}
	if targetHash == strings.ToLower(e.OldSHA256) {
		// Journal published but no destructive rename happened yet. A backup
		// alongside an intact old target is not a state this journal can
		// produce — treat it as tampering rather than deleting anything.
		if _, err := os.Lstat(e.BackupPath); err == nil {
			return &errJournalAmbiguous{
				journal: journalFile, entry: *e,
				detail: fmt.Sprintf("target %s still holds the old binary while backup %s also exists", e.Target, e.BackupPath),
			}
		}
		if err := os.Remove(e.NewPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove staged %s after pre-rename abort: %w", e.NewPath, err)
		}
		return nil
	}
	return &errJournalAmbiguous{
		journal: journalFile, entry: *e,
		detail: fmt.Sprintf("target %s hashes to %s, which is neither the recorded old (%s) nor new (%s) digest", e.Target, targetHash, e.OldSHA256, e.NewSHA256),
	}
}

// recoverMissingTarget restores the old binary from the backup, or fails
// loudly when no trustworthy backup exists.
func recoverMissingTarget(e *journalEntry, dir, journalFile string) error {
	backupHash, backupErr := fileSHA256Hex(e.BackupPath)
	switch {
	case errors.Is(backupErr, fs.ErrNotExist):
		return &errJournalAmbiguous{
			journal: journalFile, entry: *e,
			detail: fmt.Sprintf("target %s is missing and backup %s is missing too — nothing safe to restore; the staged payload %s must not be installed without the update's verification trail", e.Target, e.BackupPath, e.NewPath),
		}
	case backupErr != nil:
		return fmt.Errorf("hash journal backup %s (no recovery performed): %w", e.BackupPath, backupErr)
	}
	if backupHash != strings.ToLower(e.OldSHA256) {
		return &errJournalAmbiguous{
			journal: journalFile, entry: *e,
			detail: fmt.Sprintf("target %s is missing and backup %s hashes to %s, not the recorded old %s — refusing to restore unverified bytes", e.Target, e.BackupPath, backupHash, e.OldSHA256),
		}
	}
	if err := os.Rename(e.BackupPath, e.Target); err != nil {
		return fmt.Errorf("restore %s from backup %s (no recovery performed): %w", e.Target, e.BackupPath, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync directory after restoring %s: %w", e.Target, err)
	}
	// The install is back to its pre-update state; drop the staged payload.
	if err := os.Remove(e.NewPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove staged %s after restore: %w", e.NewPath, err)
	}
	return nil
}
