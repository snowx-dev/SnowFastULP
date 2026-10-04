package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

// H-12: the history store treats a trailing separator as a directory hint
// (the path is a directory, adopt DIR/history.sqlite3 inside it), but config
// resolution used to Clean the path and strip the hint — so a configured
// "new-history/" silently became a database FILE named new-history. The hint
// must survive resolution.
func TestApplyHistoryPreservesDirectoryHint(t *testing.T) {
	cfg := loadHistoryConfig(t, "[history]\nenabled = true\npath = \"/tmp/snowfast-h12/new-history/\"\n")
	var path string
	if err := cfg.ApplyHistory(config.Visited{}, config.HistoryFlags{Path: &path}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/") {
		t.Fatalf("resolved history path = %q, want trailing separator preserved (directory hint)", path)
	}
}

// Same contract for the backslash spelling the store also accepts.
func TestApplyHistoryPreservesBackslashHint(t *testing.T) {
	cfg := loadHistoryConfig(t, "[history]\nenabled = true\npath = 'C:\\Temp\\new-history\\'\n")
	var path string
	if err := cfg.ApplyHistory(config.Visited{}, config.HistoryFlags{Path: &path}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/") && !strings.HasSuffix(path, `\`) {
		t.Fatalf("resolved history path = %q, want trailing separator preserved (directory hint)", path)
	}
}

// No hint configured: resolution stays cleaned (no separator appended).
func TestApplyHistoryCleanWithoutHint(t *testing.T) {
	cfg := loadHistoryConfig(t, "[history]\nenabled = true\npath = \"sub/dir\"\n")
	var path string
	if err := cfg.ApplyHistory(config.Visited{}, config.HistoryFlags{Path: &path}); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cwd, "sub", "dir")
	if path != want {
		t.Fatalf("resolved history path = %q, want %q", path, want)
	}
	if strings.HasSuffix(path, "/") {
		t.Fatalf("unhinted path must not gain a separator: %q", path)
	}
}
