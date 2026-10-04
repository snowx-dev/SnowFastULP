package selfupdate

import (
	"os"
)

// canonicalizeJournalPath resolves path to its platform-canonical spelling.
// Overridable by tests so Windows-shaped spellings — two names for the same
// directory, e.g. C:\Users\RUNNER~1\... and C:\Users\runneradmin\... — can
// exercise the full comparison on any OS; the real implementations live in
// the build-tagged journal_dir_windows.go / journal_dir_other.go.
var canonicalizeJournalPath = canonicalJournalPath

// sameJournalDir reports whether entryDir is the journal directory dir.
// It is the containment check behind journalEntry.validate, and it must
// accept every spelling of the journal directory the platform can produce:
// on Windows the same directory is spelled long (C:\Users\runneradmin\...)
// or 8.3-short (C:\Users\RUNNER~1\...) depending on which API handed the
// path over — os.Executable and temp-dir results routinely disagree — and
// its file systems are case-insensitive. A journal whose entries were
// written under one spelling must therefore still validate when recovery
// derives the directory under the other.
//
// String comparison runs after per-platform canonicalization (8.3
// short-name expansion on Windows); if that is inconclusive and both
// directories exist, os.SameFile settles it on file-identity grounds.
// dir always exists by construction (the journal file was found in it);
// entryDir exists whenever the journal's parent does.
func sameJournalDir(dir, entryDir string) bool {
	if spellingEqual(canonicalizeJournalPath(dir), canonicalizeJournalPath(entryDir)) {
		return true
	}
	da, erra := os.Stat(dir)
	eb, errb := os.Stat(entryDir)
	return erra == nil && errb == nil && os.SameFile(da, eb)
}
