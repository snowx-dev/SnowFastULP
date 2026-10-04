package config_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestDefaultDataDirUsesXDGDataHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG data directory applies to Unix")
	}
	base := t.TempDir()
	t.Setenv("XDG_DATA_HOME", base)

	got, err := config.DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "snowfast"); got != want {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultDataDirFallsBackToHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix home fallback")
	}
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", home)

	got, err := config.DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "snowfast"); got != want {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultHistoryPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		base := t.TempDir()
		t.Setenv("XDG_DATA_HOME", base)
	}
	got, err := config.DefaultHistoryPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "history.sqlite3" {
		t.Fatalf("DefaultHistoryPath() = %q, want history.sqlite3 suffix", got)
	}
	dir, err := config.DefaultDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "history.sqlite3"); got != want {
		t.Fatalf("DefaultHistoryPath() = %q, want %q", got, want)
	}
}
