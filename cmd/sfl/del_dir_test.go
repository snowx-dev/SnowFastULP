package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A non-empty tdata-shaped directory is a direct child of the input root that
// is discovered as a single source. Before the directory-branch fix, such a
// group was a direct file child (isChild) and deleteParsedSources took the
// regular-file branch: os.Remove failed on the non-empty directory with
// ENOTEMPTY, aborting deletion of every later group. It must be removed
// recursively with os.RemoveAll, and processing/deletion of later groups must
// continue.
func TestRunDeletesNonEmptyTdataDirAndContinuesLaterGroups(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")

	// tdata-shaped and a DIRECT child of the input root: directory named
	// tdata with a key_datas regular file plus a nested cache tree,
	// discovered whole under -env.
	tdataDir := filepath.Join(input, "tdata")
	if err := os.MkdirAll(filepath.Join(tdataDir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tdataDir, "key_datas"), "keydata\n")
	writeFile(t, filepath.Join(tdataDir, "cache", "maps0"), "cache-bytes\n")

	// A later group that must still be deleted after the tdata dir succeeds.
	writeFile(t, filepath.Join(input, "victimB", "Passwords.txt"), "URL: b.com\nUSER: u2\nPASS: p2\n")

	outDir := filepath.Join(dir, "out")
	if err := run(runConfig{
		Input: input, OutputDir: outDir, Workers: 2, NoTUI: true,
		DeleteSources: true, Env: true,
		RunStamp: "20260920_120000",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(tdataDir); !os.IsNotExist(err) {
		t.Fatalf("tdata-shaped directory should be deleted recursively: %v", err)
	}
	if _, err := os.Stat(filepath.Join(input, "victimB")); !os.IsNotExist(err) {
		t.Fatalf("later group victimB should still be deleted: %v", err)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("input root must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "sfl_20260920_120000.txt")); err != nil {
		t.Fatalf("output must survive: %v", err)
	}
}
