package config_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestApplySFSStatsFromTOML(t *testing.T) {
	f := loadSFSConfig(t, "[sfs]\nstats = true\n")
	stats := false

	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{Stats: &stats}); err != nil {
		t.Fatal(err)
	}
	if !stats {
		t.Fatal("expected stats=true from TOML")
	}
}

func TestApplySFSStatsCLIWinsOverConfig(t *testing.T) {
	f := loadSFSConfig(t, "[sfs]\nstats = true\n")
	fs := flag.NewFlagSet("sfs", flag.ContinueOnError)
	stats := fs.Bool("stats", false, "")
	if err := fs.Parse([]string{"-stats=false"}); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	if err := f.ApplySFS(visited, config.SFSFlags{Stats: stats}); err != nil {
		t.Fatal(err)
	}
	if *stats {
		t.Fatal("explicit CLI -stats=false should override config stats=true")
	}
}

// Legacy [sfs].stream / [sfs].silent are accepted for parse-compat but are
// mode no-ops now that stream is the default. Config stats=true still applies
// even when the user visits legacy -s on the CLI; stream/silent have no
// effect on mode resolution.
func TestApplySFSStatsConfigWithStreamCLIStillSetsStats(t *testing.T) {
	f := loadSFSConfig(t, "[sfs]\nstats = true\n")
	fs := flag.NewFlagSet("sfs", flag.ContinueOnError)
	fs.Bool("s", false, "")
	if err := fs.Parse([]string{"-s"}); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	stats := false
	if err := f.ApplySFS(visited, config.SFSFlags{Stats: &stats}); err != nil {
		t.Fatal(err)
	}
	if !stats {
		t.Fatal("config stats=true should still apply when only -s is visited")
	}
}

func loadSFSConfig(t *testing.T, content string) config.File {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
