package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestODDryRunMissingSidecarParity (M-06, red-first): a valid library
// archive whose .idx went missing must be counted by -odr exactly as the
// subsequent real -od counts it. Pre-fix, the dry run omitted the part and
// reported unique=5 / skippedByDest=0 where the real run reported 2/3.
func TestODDryRunMissingSidecarParity(t *testing.T) {
	stage := t.TempDir()
	libDir := t.TempDir()

	run1Input := filepath.Join(t.TempDir(), "in1.txt")
	writeFileContent(t, run1Input, strings.Join([]string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
		"https://c.example.com:user3:pw3",
		"https://d.example.com:user4:pw4",
		"https://e.example.com:user5:pw5",
	}, "\n")+"\n")
	runODOnce(t, libDir, filepath.Join(stage, "s1"), run1Input, "stamp_one", false)

	// simulate the lost index
	arch := filepath.Join(libDir, "sfu_stamp_one.txt.zst")
	if _, err := os.Stat(arch); err != nil {
		t.Fatalf("run1 archive missing: %v", err)
	}
	if err := os.Remove(sidecarPathForArchive(arch)); err != nil {
		t.Fatal(err)
	}

	// 3 dups with run1 + 2 new
	run2Input := filepath.Join(t.TempDir(), "in2.txt")
	writeFileContent(t, run2Input, strings.Join([]string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
		"https://c.example.com:user3:pw3",
		"https://f.example.com:user6:pw6",
		"https://g.example.com:user7:pw7",
	}, "\n")+"\n")

	before := libraryFingerprint(t, libDir)
	mDry := runODOnce(t, libDir, filepath.Join(stage, "s2"), run2Input, "stamp_two", true)
	after := libraryFingerprint(t, libDir)
	if len(after) != len(before) {
		t.Errorf("dry-run changed library file count: before=%d after=%d", len(before), len(after))
	}
	for rel, sum := range before {
		if after[rel] != sum {
			t.Errorf("dry-run changed library file %q", rel)
		}
	}

	// the real -od the preview must match
	mReal := runODOnce(t, libDir, filepath.Join(stage, "s3"), run2Input, "stamp_three", false)

	if got, want := mDry.LinesSkippedByDest.Load(), mReal.LinesSkippedByDest.Load(); got != want {
		t.Errorf("dry-run linesSkippedByDest = %d, real -od = %d (want equal, 3)", got, want)
	}
	if got := mDry.LinesSkippedByDest.Load(); got != 3 {
		t.Errorf("dry-run linesSkippedByDest = %d, want 3 (part's real keys counted)", got)
	}
	if got, want := mDry.LinesUnique.Load(), mReal.LinesUnique.Load(); got != want {
		t.Errorf("dry-run linesUnique = %d, real -od = %d (want equal, 2)", got, want)
	}
	if got := mDry.LinesUnique.Load(); got != 2 {
		t.Errorf("dry-run linesUnique = %d, want 2", got)
	}
}
