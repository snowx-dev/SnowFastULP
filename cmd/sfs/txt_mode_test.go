package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestRunTxtModeWritesHits(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	f2 := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(f1, []byte("alpha\nneedle line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "hits.txt")
	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:     dir,
		pattern:  "needle",
		archives: []string{f1, f2},
		txtMode:  true,
		workers:  2,
		outFile:  outPath,
		silent:   true,
		started:  time.Now(),
		metrics:  metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "needle") {
		t.Fatalf("output %q missing needle", data)
	}
	if metrics.Hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", metrics.Hits.Load())
	}
}

func TestConfigTxtMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfs]\ntxt = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	txt := false
	visited := config.Visited{}
	if err := f.ApplySFS(visited, config.SFSFlags{Txt: &txt}); err != nil {
		t.Fatal(err)
	}
	if !txt {
		t.Fatal("expected txt=true from config")
	}
}
