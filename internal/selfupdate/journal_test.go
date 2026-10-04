package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- pure journal state tests (platform-neutral) ---

func TestJournalEntryValidate(t *testing.T) {
	dir := t.TempDir()
	valid := func() *journalEntry {
		return &journalEntry{
			Target:     filepath.Join(dir, "sfu.exe"),
			NewPath:    filepath.Join(dir, ".sfu.exe.new-0102"),
			BackupPath: filepath.Join(dir, ".sfu.exe.old-0304"),
			NewSHA256:  strings.Repeat("a", 64),
			OldSHA256:  strings.Repeat("b", 64),
			State:      journalStatePending,
		}
	}
	if err := valid().validate(dir); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	mutate := func(f func(e *journalEntry)) error {
		e := valid()
		f(e)
		return e.validate(dir)
	}
	if mutate(func(e *journalEntry) { e.Target = "" }) == nil {
		t.Fatal("empty target accepted")
	}
	if mutate(func(e *journalEntry) { e.Target = "sfu.exe" }) == nil {
		t.Fatal("relative target accepted")
	}
	if mutate(func(e *journalEntry) { e.NewPath = filepath.Join(dir, "other", ".x.new-1") }) == nil {
		t.Fatal(".new outside the journal directory accepted")
	}
	if mutate(func(e *journalEntry) { e.BackupPath = e.NewPath }) == nil {
		t.Fatal("aliasing .new/.old accepted")
	}
	if mutate(func(e *journalEntry) { e.NewSHA256 = "nothex" }) == nil {
		t.Fatal("bad new hash accepted")
	}
	if mutate(func(e *journalEntry) { e.OldSHA256 = "" }) == nil {
		t.Fatal("missing old hash accepted")
	}
	if mutate(func(e *journalEntry) { e.State = "mysterious" }) == nil {
		t.Fatal("unknown state accepted")
	}
	if err := (&updateJournal{Version: 2}).validate(dir); err == nil {
		t.Fatal("unknown journal version accepted")
	}
}

func TestParseJournalCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, err := parseJournal([]byte("not json"), filepath.Join(dir, journalFileName)); err == nil {
		t.Fatal("garbage accepted")
	}
	bad := `{"version":2,"entries":[]}`
	if _, err := parseJournal([]byte(bad), filepath.Join(dir, journalFileName)); err == nil {
		t.Fatal("wrong version accepted")
	}
	missing := `{"version":1,"entries":[{"target":"x"}]}`
	if _, err := parseJournal([]byte(missing), filepath.Join(dir, journalFileName)); err == nil {
		t.Fatal("incomplete entry accepted")
	}
}

func TestWriteJournalAtomicallyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	j := &updateJournal{Version: journalVersion, Entries: []journalEntry{{
		Target:     filepath.Join(dir, "sfu.exe"),
		NewPath:    filepath.Join(dir, ".sfu.exe.new-1"),
		BackupPath: filepath.Join(dir, ".sfu.exe.old-2"),
		NewSHA256:  strings.Repeat("a", 64),
		OldSHA256:  strings.Repeat("b", 64),
		State:      journalStatePending,
	}}}
	if err := writeJournalAtomically(dir, j); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, journalFileName))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	parsed, err := parseJournal(data, filepath.Join(dir, journalFileName))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Entries) != 1 || parsed.Entries[0].Target != j.Entries[0].Target {
		t.Fatalf("round trip mismatch: %+v", parsed)
	}
	// Rewriting (publish over an existing journal) must work and leave no temps.
	if err := writeJournalAtomically(dir, j); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(dir, journalFileName+".tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp journal files left behind: %v", entries)
	}
}

// --- crash-window recovery tests via the fault hooks ---

// journalFixture builds an install dir with one existing binary and returns
// the pending list + payloads for stageReplaceJournal.
func journalFixture(t *testing.T, names ...string) (dir string, pending []pendingUpdate, payloads [][]byte, old map[string][]byte) {
	t.Helper()
	dir = t.TempDir()
	old = map[string][]byte{}
	for i, name := range names {
		oldContent := []byte(fmt.Sprintf("old-%s-%d", name, i))
		newContent := []byte(fmt.Sprintf("new-%s-%d", name, i))
		target := filepath.Join(dir, name+exeExt())
		if err := os.WriteFile(target, oldContent, 0o755); err != nil {
			t.Fatal(err)
		}
		old[name] = oldContent
		newSum := sha256.Sum256(newContent)
		pending = append(pending, pendingUpdate{bin: name, target: target, isNew: false, hash: newSum[:]})
		payloads = append(payloads, newContent)
	}
	return dir, pending, payloads, old
}

func globSwapArtifacts(t *testing.T, dir string) []string {
	t.Helper()
	var all []string
	for _, pattern := range []string{"*.new-*", "*.old-*", journalFileName} {
		m, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, m...)
	}
	return all
}

func TestRecoverAbortsCleanlyBeforeFirstRename(t *testing.T) {
	dir, pending, payloads, old := journalFixture(t, "sfu")
	// Crash right after the journal is published, before any rename.
	journalFaultHook = func(p journalFaultPoint, _ *journalEntry) error {
		if p == faultAfterJournalPublish {
			return errors.New("simulated crash")
		}
		return nil
	}
	t.Cleanup(func() { journalFaultHook = nil })

	_, err := stageReplaceJournal(dir, pending, payloads)
	if err == nil {
		t.Fatal("expected the simulated crash to surface")
	}
	target := pending[0].target
	assertFileContents(t, target, string(old["sfu"])) // target intact
	if _, err := os.Stat(filepath.Join(dir, journalFileName)); err != nil {
		t.Fatalf("journal must exist after publish: %v", err)
	}

	if err := recoverInterruptedUpdate(dir); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	assertFileContents(t, target, string(old["sfu"]))
	if left := globSwapArtifacts(t, dir); len(left) != 0 {
		t.Fatalf("recovery left swap artifacts: %v", left)
	}
}

func TestRecoverRestoresBackupWhenTargetMissing(t *testing.T) {
	dir, pending, payloads, old := journalFixture(t, "sfu", "sfs")
	journalFaultHook = func(p journalFaultPoint, _ *journalEntry) error {
		if p == faultAfterTargetBackup {
			return errors.New("simulated crash")
		}
		return nil
	}
	t.Cleanup(func() { journalFaultHook = nil })

	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	// First target swaps through target→backup, then "crashes".
	if err := swapTarget(rj.entry(pending[0].target), dir); err == nil {
		t.Fatal("expected simulated crash from swapTarget")
	}
	target := pending[0].target
	if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target should be missing mid-swap, err=%v", err)
	}

	// Simulated restart: recovery must bring the old binary back.
	if err := recoverInterruptedUpdate(dir); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	assertFileContents(t, target, string(old["sfu"]))
	assertFileContents(t, pending[1].target, string(old["sfs"])) // untouched sibling
	if left := globSwapArtifacts(t, dir); len(left) != 0 {
		t.Fatalf("recovery left swap artifacts: %v", left)
	}
}

func TestRecoverAcceptsVerifiedNewTarget(t *testing.T) {
	dir, pending, payloads, _ := journalFixture(t, "sfu")
	journalFaultHook = func(p journalFaultPoint, _ *journalEntry) error {
		if p == faultAfterNewTarget {
			return errors.New("simulated crash")
		}
		return nil
	}
	t.Cleanup(func() { journalFaultHook = nil })

	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if err := swapTarget(rj.entry(pending[0].target), dir); err == nil {
		t.Fatal("expected simulated crash from swapTarget")
	}
	target := pending[0].target
	// The new binary is in place; only the backup/journal cleanup was lost.
	assertFileContents(t, target, string(payloads[0]))

	if err := recoverInterruptedUpdate(dir); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	assertFileContents(t, target, string(payloads[0])) // verified new survives
	if left := globSwapArtifacts(t, dir); len(left) != 0 {
		t.Fatalf("recovery left swap artifacts: %v", left)
	}
}

func TestSwapFailureRestoresBackupViaRecovery(t *testing.T) {
	dir, pending, payloads, old := journalFixture(t, "sfu")
	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	e := rj.entry(pending[0].target)
	// Make the second rename fail: the staged .new vanishes before the swap.
	if err := os.Remove(e.NewPath); err != nil {
		t.Fatal(err)
	}
	if err := swapTarget(e, dir); err == nil {
		t.Fatal("expected swap failure when the staged payload is gone")
	}
	// The live failure path repairs through the same recovery as a restart.
	if err := recoverInterruptedUpdate(dir); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	assertFileContents(t, pending[0].target, string(old["sfu"]))
	if left := globSwapArtifacts(t, dir); len(left) != 0 {
		t.Fatalf("recovery left swap artifacts: %v", left)
	}
}

// --- ambiguous / corrupt journals fail loudly and preserve every file ---

func writeTestJournal(t *testing.T, dir string, entries []journalEntry) {
	t.Helper()
	if err := writeJournalAtomically(dir, &updateJournal{Version: journalVersion, Entries: entries}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverAmbiguousForeignTargetFailsLoud(t *testing.T) {
	dir, pending, payloads, old := journalFixture(t, "sfu")
	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err != nil {
		t.Fatal(err)
	}
	e := rj.entry(pending[0].target)
	// Crash-shaped state: target→backup happened, then someone replaced the
	// target with foreign bytes while the backup exists.
	if err := os.Rename(e.Target, e.BackupPath); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("neither old nor new")
	if err := os.WriteFile(e.Target, foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	err = recoverInterruptedUpdate(dir)
	if err == nil {
		t.Fatal("expected loud failure for an ambiguous target")
	}
	for _, want := range []string{e.Target, e.BackupPath, e.NewPath, filepath.Join(dir, journalFileName)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %s", err, want)
		}
	}
	// Nothing may be deleted.
	assertFileContents(t, e.Target, string(foreign))
	assertFileContents(t, e.BackupPath, string(old["sfu"]))
	if _, err := os.Stat(e.NewPath); err != nil {
		t.Fatalf("staged .new must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, journalFileName)); err != nil {
		t.Fatalf("journal must survive: %v", err)
	}
}

func TestRecoverTargetMissingNoBackupFailsLoud(t *testing.T) {
	dir, pending, payloads, _ := journalFixture(t, "sfu")
	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err != nil {
		t.Fatal(err)
	}
	e := rj.entry(pending[0].target)
	// Crash-shaped state no rename sequence can produce: target AND backup
	// both gone, staged .new still present.
	if err := os.Rename(e.Target, e.BackupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.BackupPath); err != nil {
		t.Fatal(err)
	}
	err = recoverInterruptedUpdate(dir)
	if err == nil {
		t.Fatal("expected loud failure when neither target nor backup exists")
	}
	if _, err := os.Stat(e.NewPath); err != nil {
		t.Fatalf("staged .new must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, journalFileName)); err != nil {
		t.Fatalf("journal must survive: %v", err)
	}
}

func TestRecoverCorruptJournalFailsLoud(t *testing.T) {
	dir, _, _, old := journalFixture(t, "sfu")
	journalPath := filepath.Join(dir, journalFileName)
	if err := os.WriteFile(journalPath, []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedUpdate(dir); err == nil {
		t.Fatal("expected loud failure for a corrupt journal")
	}
	target := filepath.Join(dir, "sfu"+exeExt())
	assertFileContents(t, target, string(old["sfu"]))
}

func TestRecoverNoJournalIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := recoverInterruptedUpdate(dir); err != nil {
		t.Fatalf("recovery without a journal: %v", err)
	}
}

func TestRecoverBackupHashMismatchFailsLoud(t *testing.T) {
	dir, pending, payloads, _ := journalFixture(t, "sfu")
	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err != nil {
		t.Fatal(err)
	}
	e := rj.entry(pending[0].target)
	// Target missing, but the backup no longer holds the recorded old bytes.
	if err := os.Rename(e.Target, e.BackupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.BackupPath, []byte("tampered backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedUpdate(dir); err == nil {
		t.Fatal("expected loud failure when the backup does not match the recorded old hash")
	}
	if _, err := os.Stat(e.BackupPath); err != nil {
		t.Fatalf("tampered backup must be preserved: %v", err)
	}
	if _, err := os.Stat(e.NewPath); err != nil {
		t.Fatalf("staged .new must survive: %v", err)
	}
}

// --- unique swap path allocation ---

func TestUniqueJournalPath(t *testing.T) {
	dir := t.TempDir()
	first, err := uniqueJournalPath(dir, "sfu"+exeExt(), "new")
	if err != nil {
		t.Fatal(err)
	}
	second, err := uniqueJournalPath(dir, "sfu"+exeExt(), "new")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two allocations collided: %s", first)
	}
	if filepath.Dir(first) != dir || !strings.HasPrefix(filepath.Base(first), ".sfu"+exeExt()+".new-") {
		t.Fatalf("unexpected shape: %s", first)
	}
}

// --- staging failure cleans up and never publishes ---

func TestStageFailureCleansUpAndPublishesNothing(t *testing.T) {
	dir, _, _, _ := journalFixture(t, "sfu")
	// Second target is missing on disk → old-hash computation fails after the
	// first target's .new was already staged.
	pending := []pendingUpdate{
		{bin: "sfu", target: filepath.Join(dir, "sfu"+exeExt()), isNew: false},
		{bin: "sfs", target: filepath.Join(dir, "sfs"+exeExt()), isNew: false},
	}
	payloads := [][]byte{[]byte("new-sfu"), []byte("new-sfs")}
	rj, err := stageReplaceJournal(dir, pending, payloads)
	if err == nil {
		t.Fatal("expected staging failure for the missing target")
	}
	if rj != nil {
		t.Fatal("no journal handle may be returned on failure")
	}
	if _, err := os.Stat(filepath.Join(dir, journalFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("journal must not be published on staging failure, err=%v", err)
	}
	if left := globSwapArtifacts(t, dir); len(left) != 0 {
		t.Fatalf("staging failure left artifacts: %v", left)
	}
}

// --- staging sweeps leftovers from earlier crashed/hidden runs ---

func TestStageSweepsStaleArtifacts(t *testing.T) {
	dir, pending, payloads, _ := journalFixture(t, "sfu")
	stale := []string{
		filepath.Join(dir, ".sfu"+exeExt()+".old-deadbeef"),
		filepath.Join(dir, ".sfu"+exeExt()+".new-deadbeef"),
		filepath.Join(dir, journalFileName+".tmp-123"),
	}
	for _, p := range stale {
		if err := os.WriteFile(p, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stageReplaceJournal(dir, pending, payloads); err != nil {
		t.Fatalf("stage: %v", err)
	}
	for _, p := range stale {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stale artifact %s not swept, err=%v", p, err)
		}
	}
}

// --- run()-level integration: recovery precedes target inspection ---

func TestRunHealsMissingTargetFromBackupBeforePlanning(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-0.1.2\n")
	newSFS := []byte("#!/bin/sh\necho sfs-0.1.2\n")
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, newSFU, newSFS)
	defer srv.Close()

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	sfuTarget := filepath.Join(dir, "sfu"+exeExt())
	fixture := fixtureBytes(t)

	// Crash-shaped state: sfu.exe is missing, its backup holds the old
	// binary, and a journal describes the interrupted swap.
	backup := filepath.Join(dir, ".sfu"+exeExt()+".old-cafe")
	if err := os.WriteFile(backup, fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sfuTarget); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fixture)
	sumNew := sha256.Sum256(newSFU)
	j := &updateJournal{Version: journalVersion, Entries: []journalEntry{{
		Target:     sfuTarget,
		NewPath:    filepath.Join(dir, ".sfu"+exeExt()+".new-beef"),
		BackupPath: backup,
		NewSHA256:  hex.EncodeToString(sumNew[:]),
		OldSHA256:  hex.EncodeToString(sum[:]),
		State:      journalStatePending,
	}}}
	if err := writeJournalAtomically(dir, j); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run after recovery: %v", err)
	}
	assertFileBytes(t, sfuTarget, newSFU)
	assertFileBytes(t, filepath.Join(dir, "sfs"+exeExt()), newSFS)
	if _, err := os.Stat(backup); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("backup should be gone after the update, err=%v", err)
	}
}

func TestRunHealsAbortedJournalBeforePlanning(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-0.1.2\n")
	newSFS := []byte("#!/bin/sh\necho sfs-0.1.2\n")
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, newSFU, newSFS)
	defer srv.Close()

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	sfuTarget := filepath.Join(dir, "sfu"+exeExt())
	fixture := fixtureBytes(t)

	// Journal published, .new staged, but no rename happened yet.
	staged := filepath.Join(dir, ".sfu"+exeExt()+".new-beef")
	if err := os.WriteFile(staged, newSFU, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fixture)
	sumNew := sha256.Sum256(newSFU)
	j := &updateJournal{Version: journalVersion, Entries: []journalEntry{{
		Target:     sfuTarget,
		NewPath:    filepath.Join(dir, ".sfu"+exeExt()+".new-beef"),
		BackupPath: filepath.Join(dir, ".sfu"+exeExt()+".old-beef"),
		NewSHA256:  hex.EncodeToString(sumNew[:]),
		OldSHA256:  hex.EncodeToString(sum[:]),
		State:      journalStatePending,
	}}}
	if err := writeJournalAtomically(dir, j); err != nil {
		t.Fatal(err)
	}

	if err := run(nil, "0.1.1", "sfu", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertFileBytes(t, sfuTarget, newSFU)
	if left := globSwapArtifacts(t, dir); len(left) != 0 {
		t.Fatalf("run left journal/swap artifacts: %v", left)
	}
}
