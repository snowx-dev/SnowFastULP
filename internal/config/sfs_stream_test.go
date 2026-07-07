package config_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/config"
)

func TestApplySFSStreamFromTOML(t *testing.T) {
	f := loadSFSConfig(t, "[sfs]\nstream = true\n")
	stream := false

	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{Stream: &stream}); err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("expected stream=true from TOML")
	}
}

func TestApplySFSSilentAliasSetsStream(t *testing.T) {
	f := loadSFSConfig(t, "[sfs]\nsilent = true\n")
	stream := false

	if err := f.ApplySFS(config.Visited{}, config.SFSFlags{Stream: &stream}); err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("expected legacy silent=true to enable stream mode")
	}
}

func TestApplySFSStreamCLIWinsOverSilentAlias(t *testing.T) {
	f := loadSFSConfig(t, "[sfs]\nsilent = true\n")
	fs := flag.NewFlagSet("sfs", flag.ContinueOnError)
	stream := fs.Bool("s", false, "")
	if err := fs.Parse([]string{"-s=false"}); err != nil {
		t.Fatal(err)
	}
	visited := config.Visited{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })

	if err := f.ApplySFS(visited, config.SFSFlags{Stream: stream}); err != nil {
		t.Fatal(err)
	}
	if *stream {
		t.Fatal("explicit CLI -s=false should override legacy silent=true")
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
