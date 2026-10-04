package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupPathForTreatsDanglingSymlinkAsOccupied(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	now := time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC)
	first := backupPathForAt(cfgPath, now)
	if err := os.Symlink(filepath.Join(dir, "missing-target"), first); err != nil {
		t.Skipf("create dangling symlink: %v", err)
	}
	want := filepath.Join(dir, "config.toml.20260929T220000Z-2.bak")
	if got := backupPathForAt(cfgPath, now); got != want {
		t.Fatalf("backup path = %q, want %q after dangling symlink", got, want)
	}
}
