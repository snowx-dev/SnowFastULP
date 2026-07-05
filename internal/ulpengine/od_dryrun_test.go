package ulpengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// runODOnce is the shared -od harness from od_integration_test.go, factored
// here so the dry-run parity check can build a real library then preview
// against it with identical plumbing.
func runODOnce(t *testing.T, libDir, stageDir, input, stamp string, dryRun bool) *Metrics {
	t.Helper()
	r, err := Resolve(Config{
		Inputs:       []string{input},
		Output:       filepath.Join(libDir, "sfu_"+stamp+".txt.zst"),
		TempDir:      stageDir,
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		DryRun:       dryRun,
		RunStamp:     stamp,
	})
	if err != nil {
		t.Fatalf("resolve %s: %v", stamp, err)
	}
	m := &Metrics{}
	if err := Run(context.Background(), &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      stageDir,
	}, m); err != nil {
		t.Fatalf("run %s: %v", stamp, err)
	}
	return m
}

// libraryFingerprint hashes every regular file under dir (relpath -> sha256),
// excluding nothing: a dry-run must leave the library byte-identical, so any
// change — new archive, rewritten .idx, v2→v3 upgrade — is a failure.
func libraryFingerprint(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("fingerprint %s: %v", dir, err)
	}
	return out
}

// TestODDryRunParityAndNoWrite: a -odr run reports the same new/dup counts a
// real -od run would, but leaves the library untouched (no new .zst, no .idx
// change, no v2→v3 upgrade) and does not delete the input.
func TestODDryRunParityAndNoWrite(t *testing.T) {
	libDir := t.TempDir()
	stage := t.TempDir()

	// run1: real -od, 5 creds -> library now holds one archive + sidecar.
	run1Input := filepath.Join(t.TempDir(), "in1.txt")
	writeFileContent(t, run1Input, strings.Join([]string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
		"https://c.example.com:user3:pw3",
		"https://d.example.com:user4:pw4",
		"https://e.example.com:user5:pw5",
	}, "\n")+"\n")
	runODOnce(t, libDir, filepath.Join(stage, "s1"), run1Input, "stamp_one", false)

	before := libraryFingerprint(t, libDir)

	// run2 input: 3 dups with run1 + 2 new. Same input a real -od run would
	// see, so the dry-run's stats must match TestODTwoRunDedup's (2 new, 3 dup).
	run2Input := filepath.Join(t.TempDir(), "in2.txt")
	writeFileContent(t, run2Input, strings.Join([]string{
		"https://a.example.com:user1:pw1", // dup
		"https://b.example.com:user2:pw2", // dup
		"https://c.example.com:user3:pw3", // dup
		"https://f.example.com:user6:pw6", // new
		"https://g.example.com:user7:pw7", // new
	}, "\n")+"\n")

	m := runODOnce(t, libDir, filepath.Join(stage, "s2"), run2Input, "stamp_two", true)

	if got := m.LinesUnique.Load(); got != 2 {
		t.Errorf("dry-run linesUnique = %d, want 2 (parity with real -od)", got)
	}
	if got := m.LinesSkippedByDest.Load(); got != 3 {
		t.Errorf("dry-run linesSkippedByDest = %d, want 3 (parity with real -od)", got)
	}

	// No new archive named after the dry-run stamp landed in the library.
	if _, err := os.Stat(filepath.Join(libDir, "sfu_stamp_two.txt.zst")); err == nil {
		t.Error("dry-run wrote an archive to the library; it must write nothing")
	}

	// Library byte-identical: no new files, no .idx edits, no v2→v3 upgrade.
	after := libraryFingerprint(t, libDir)
	if len(before) != len(after) {
		t.Errorf("library file count changed: before=%d after=%d", len(before), len(after))
	}
	keys := make([]string, 0, len(before))
	for k := range before {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if before[k] != after[k] {
			t.Errorf("library file %q changed (or appeared/disappeared): before=%s after=%s",
				k, before[k][:8], after[k][:8])
		}
	}

	// -del is suppressed in dry-run: the input file must still exist.
	if _, err := os.Stat(run2Input); err != nil {
		t.Errorf("dry-run deleted the input file; -del must be suppressed: %v", err)
	}
}
