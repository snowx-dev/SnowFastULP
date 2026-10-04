package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func loadHistoryConfig(t *testing.T, body string) config.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestLoadHistory(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		cfg := loadHistoryConfig(t, "[sfu]\n")
		if cfg.History.Enabled != nil || cfg.History.Path != "" {
			t.Fatalf("History = %+v, want zero value", cfg.History)
		}
	})

	t.Run("values", func(t *testing.T) {
		cfg := loadHistoryConfig(t, "[history]\nenabled = true\npath = \"history.db\"\n")
		if cfg.History.Enabled == nil || !*cfg.History.Enabled {
			t.Fatalf("History.Enabled = %v, want true", cfg.History.Enabled)
		}
		if cfg.History.Path != "history.db" {
			t.Fatalf("History.Path = %q, want history.db", cfg.History.Path)
		}
	})
}

func TestApplyHistory(t *testing.T) {
	t.Run("configured enabled applies", func(t *testing.T) {
		cfg := loadHistoryConfig(t, "[history]\nenabled = true\n")
		enabled := false
		if err := cfg.ApplyHistory(config.Visited{}, config.HistoryFlags{Enabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		if !enabled {
			t.Fatal("configured history enabled was not applied")
		}
	})

	t.Run("explicit false wins", func(t *testing.T) {
		cfg := loadHistoryConfig(t, "[history]\nenabled = true\n")
		enabled := false
		if err := cfg.ApplyHistory(config.Visited{"history": true}, config.HistoryFlags{Enabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		if enabled {
			t.Fatal("explicit -history=false did not win")
		}
	})

	t.Run("configured path resolves", func(t *testing.T) {
		work := t.TempDir()
		t.Chdir(work)
		cfg := loadHistoryConfig(t, "[history]\npath = \"data/history.db\"\n")
		path := ""
		if err := cfg.ApplyHistory(config.Visited{}, config.HistoryFlags{Path: &path}); err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(work, "data", "history.db"); path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
	})

	t.Run("explicit path wins", func(t *testing.T) {
		cfg := loadHistoryConfig(t, "[history]\npath = \"configured.db\"\n")
		path := "/cli/history.db"
		if err := cfg.ApplyHistory(config.Visited{"history-path": true}, config.HistoryFlags{Path: &path}); err != nil {
			t.Fatal(err)
		}
		if path != "/cli/history.db" {
			t.Fatalf("path = %q, want CLI value", path)
		}
	})

	t.Run("path alone does not enable", func(t *testing.T) {
		cfg := loadHistoryConfig(t, "[history]\npath = \"configured.db\"\n")
		enabled := false
		path := ""
		if err := cfg.ApplyHistory(config.Visited{}, config.HistoryFlags{Enabled: &enabled, Path: &path}); err != nil {
			t.Fatal(err)
		}
		if enabled {
			t.Fatal("history path unexpectedly enabled history")
		}
		if path == "" {
			t.Fatal("configured path was not applied")
		}
	})

	t.Run("nil pointers are allowed", func(t *testing.T) {
		cfg := loadHistoryConfig(t, "[history]\nenabled = true\npath = \"configured.db\"\n")
		if err := cfg.ApplyHistory(config.Visited{}, config.HistoryFlags{}); err != nil {
			t.Fatal(err)
		}
	})
}
