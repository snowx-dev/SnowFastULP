package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSweepStaleTempDirsRemovesOldOrphansAndKeepsOthers(t *testing.T) {
	parent := t.TempDir()

	orphan1 := filepath.Join(parent, tempSubdirPrefix+"20240101-000000-1234")
	orphan2 := filepath.Join(parent, tempSubdirPrefix+"20240101-000001-5678")
	recent := filepath.Join(parent, tempSubdirPrefix+"20260514-000001-9999")
	keep := filepath.Join(parent, "user-data")
	for _, d := range []string{orphan1, orphan2, recent, keep} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "junk"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-(staleTempDirAge + time.Hour))
	for _, d := range []string{orphan1, orphan2} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if got := SweepStaleTempDirs(parent, ""); got != 2 {
		t.Fatalf("swept %d, want 2", got)
	}
	if _, err := os.Stat(orphan1); !os.IsNotExist(err) {
		t.Fatalf("orphan1 not removed: %v", err)
	}
	if _, err := os.Stat(orphan2); !os.IsNotExist(err) {
		t.Fatalf("orphan2 not removed: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent sfu temp dir should be kept: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-sfu dir was removed: %v", err)
	}
}

func TestPrepareTempDirCreatesParentAndSubdir(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "deep", "nested", "tempdir")
	sub, err := PrepareTempDir(parent, "20260514_tst001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(sub), tempSubdirPrefix) {
		t.Fatalf("new subdir name %q lacks prefix %q", sub, tempSubdirPrefix)
	}
	if !strings.Contains(filepath.Base(sub), "20260514_tst001") {
		t.Fatalf("subdir name %q should embed the run stamp", filepath.Base(sub))
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatal(err)
	}
}

func TestSweepStaleTempDirsExcludesCurrent(t *testing.T) {
	parent := t.TempDir()
	current := tempSubdirPrefix + "20240101-000000-1"
	other := tempSubdirPrefix + "20240101-000000-2"
	for _, d := range []string{current, other} {
		if err := os.Mkdir(filepath.Join(parent, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-(staleTempDirAge + time.Hour))
	for _, d := range []string{current, other} {
		if err := os.Chtimes(filepath.Join(parent, d), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if got := SweepStaleTempDirs(parent, current); got != 1 {
		t.Fatalf("swept %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(parent, current)); err != nil {
		t.Fatalf("excluded subdir was removed: %v", err)
	}
}
