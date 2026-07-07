package main

import (
	"path/filepath"
	"testing"
)

// All -del tests run against COPIES in t.TempDir(); the real data is never
// mutated. The output dir is always a separate temp tree so it never falls in
// the -del scope.

func TestRealDataDelArchiveRemoved(t *testing.T) {
	root := realDataRoot(t)
	src := requireFile(t, filepath.Join(fixturesDir(t, root), "plain.zip"))

	work := t.TempDir()
	arc := filepath.Join(work, "plain.zip")
	copyFile(t, src, arc)

	if err := run(runConfig{
		Input: arc, OutputDir: t.TempDir(), DeleteSources: true,
		Workers: 1, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
	}); err != nil {
		t.Fatal(err)
	}
	if exists(arc) {
		t.Fatalf("parsed archive %s should have been deleted", arc)
	}
}

func TestRealDataDelNestedVictimsRemoved(t *testing.T) {
	root := realDataRoot(t)
	input := t.TempDir()
	n := copyNVictims(t, victimsParent(t, root), input, 3)
	if n < 2 {
		t.Skip("need >=2 victim folders")
	}
	before := childDirs(t, input)

	if err := run(runConfig{
		Input: input, OutputDir: t.TempDir(), DeleteSources: true,
		Workers: 2, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
	}); err != nil {
		t.Fatal(err)
	}

	// The passed root is never removed; its parsed victim children are.
	if !exists(input) {
		t.Fatal("input root must not be deleted")
	}
	after := childDirs(t, input)
	if len(after) >= len(before) {
		t.Fatalf("no victim folders deleted: before=%d after=%d", len(before), len(after))
	}
}

func TestRealDataDelSingleRootKept(t *testing.T) {
	root := realDataRoot(t)
	input := filepath.Join(t.TempDir(), "victim")
	copyTree(t, firstVictimFolder(t, root), input)

	if err := run(runConfig{
		Input: input, OutputDir: t.TempDir(), DeleteSources: true,
		Workers: 2, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
	}); err != nil {
		t.Fatal(err)
	}
	// User-confirmed semantics: the root the user pointed at is never removed.
	if !exists(input) {
		t.Fatalf("single-root input %s must not be deleted", input)
	}
}

func TestRealDataDelBadPasswordKept(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	pwFile := requireFile(t, filepath.Join(fx, "passwords-bad-only.txt"))

	work := t.TempDir()
	arc := filepath.Join(work, "encrypted.zip")
	copyFile(t, filepath.Join(fx, "encrypted.zip"), arc)

	if err := run(runConfig{
		Input: arc, OutputDir: t.TempDir(), Password: pwFile, DeleteSources: true,
		Workers: 1, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
	}); err != nil {
		t.Fatal(err)
	}
	if !exists(arc) {
		t.Fatalf("archive that failed password should be kept: %s", arc)
	}
}
