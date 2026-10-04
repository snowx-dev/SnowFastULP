package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSameJournalDirAcceptsEquivalentSpellings pins the containment check
// against Windows-shaped input where one directory is spelled long in the
// journal path and 8.3-short in the other (the windows-latest CI failure:
// t.TempDir() handed the journal its RUNNER~1 spelling while the updater
// derived the same directory via EvalSymlinks as runneradmin, and the old
// lexical compare rejected a backup_path that was inside the journal
// directory). canonicalizeJournalPath is swapped for a fake 8.3 expander so
// the exact shape of that failure — two spellings, one directory — runs on
// any OS; the real GetLongPathNameW path is pinned on Windows by
// TestSameJournalDirWindowsShortNames.
func TestSameJournalDirAcceptsEquivalentSpellings(t *testing.T) {
	const long = `C:\Users\runneradmin\AppData\Local\Temp\TestRunHealsAbortedJournalBeforePlanning1003159639\001`
	const short = `C:\Users\RUNNER~1\AppData\Local\Temp\TestRunHealsAbortedJournalBeforePlanning1003159639\001`
	restore := canonicalizeJournalPath
	canonicalizeJournalPath = func(p string) string {
		return strings.ReplaceAll(p, `RUNNER~1`, `runneradmin`)
	}
	t.Cleanup(func() { canonicalizeJournalPath = restore })

	if !sameJournalDir(long, short) {
		t.Fatalf("short-name spelling of the journal directory rejected: dir=%q entryDir=%q", long, short)
	}
	if !sameJournalDir(short, long) {
		t.Fatalf("long-name spelling of the journal directory rejected: dir=%q entryDir=%q", short, long)
	}
	if sameJournalDir(long, strings.TrimSuffix(long, "001")+"002") {
		t.Fatal("different directory under the same accepted parent spelling")
	}
}

// TestSameJournalDirSameFileFallback pins the os.SameFile escape hatch: when
// string canonicalization cannot unify two spellings (here a symlinked name
// for the same directory) but both directories exist and are the same file,
// containment must still be accepted. This is the belt-and-braces path for
// spellings the platform canonicalizer misses (GetLongPathNameW failure).
func TestSameJournalDirSameFileFallback(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}
	if !sameJournalDir(dir, link) {
		t.Fatalf("symlinked spelling of the journal directory rejected: dir=%q entryDir=%q", dir, link)
	}
	if sameJournalDir(dir, filepath.Join(t.TempDir(), "other")) {
		t.Fatal("unrelated directory accepted as the journal directory")
	}
}
