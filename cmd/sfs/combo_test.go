package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// comboTestConfig builds a runConfig over a .txt root with the given lines.
func comboTestConfig(t *testing.T, content string, combo bool) (runConfig, string) {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "hits.txt")
	return runConfig{
		root:     dir,
		pattern:  ":",
		archives: []string{f},
		txtMode:  true,
		workers:  2,
		outFile:  outPath,
		stream:   false,
		combo:    combo,
		started:  time.Now(),
		metrics:  &search.Metrics{},
	}, outPath
}

func TestComboRewritesHitsToLoginPassword(t *testing.T) {
	cfg, outPath := comboTestConfig(t,
		"https://example.com:user1:pw1\n"+
			"http://other.org:u2:p:with:colons\n"+
			"android://abc==@com.app:login3:pass3\n", true)
	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	want := "user1:pw1\nu2:p:with:colons\nlogin3:pass3\n"
	if got != want {
		t.Fatalf("output =\n%q\nwant\n%q", got, want)
	}
	if cfg.metrics.Hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", cfg.metrics.Hits.Load())
	}
}

func TestComboSkipsNonCredentialLinesWithNote(t *testing.T) {
	// Pattern "e" matches all three lines; the two non-credential ones must
	// be skipped without output and reported in the end-of-run note.
	cfg, outPath := comboTestConfig(t,
		"https://example.com:user1:pw1\n"+
			"some random text here\n"+
			"nothing usable here either\n", true)
	cfg.pattern = "e"
	var skipped atomic.Int64
	cfg.comboSkippedOut = &skipped
	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "user1:pw1\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	// metrics.Hits counts matched lines (not emitted output): all 3 lines
	// matched; only 1 was emitted. The skipped 2 surface via the note below.
	if cfg.metrics.Hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3 (matched lines)", cfg.metrics.Hits.Load())
	}
	if n := skipped.Load(); n != 2 {
		t.Fatalf("comboSkipped = %d, want 2", n)
	}
}

func TestComboOffKeepsRawLines(t *testing.T) {
	cfg, outPath := comboTestConfig(t, "https://example.com:user1:pw1\n", false)
	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "https://example.com:user1:pw1\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestComboConfigMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[sfs]\ncombo = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	combo := false
	visited := config.Visited{}
	if err := f.ApplySFS(visited, config.SFSFlags{Combo: &combo}); err != nil {
		t.Fatal(err)
	}
	if !combo {
		t.Fatal("expected combo=true from config")
	}
}

// TestComboZSTPathEndToEnd proves the transform on the real search pipeline
// (index-backed zst archives + ordered printer), not just the txt fallback.
func TestComboZSTPathEndToEnd(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "sample.zst")
	content := "https://example.com:user1:pw1\n" +
		"android://abc==@com.app:login3:pass3\n" +
		"not a credential line\n"
	writeZST(t, arch, []byte(content))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatalf("index: %v", err)
	}

	outPath := filepath.Join(dir, "hits.txt")
	var skipped atomic.Int64
	metrics := &search.Metrics{}
	err := run(context.Background(), runConfig{
		root:            dir,
		pattern:         "*",
		matchAll:        true,
		archives:        []string{arch},
		workers:         2,
		outFile:         outPath,
		stream:          false,
		combo:           true,
		started:         time.Now(),
		metrics:         metrics,
		comboSkippedOut: &skipped,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "user1:pw1\nlogin3:pass3\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if got := metrics.Hits.Load(); got != 3 {
		t.Errorf("matched hits = %d, want 3", got)
	}
	if got := skipped.Load(); got != 1 {
		t.Errorf("combo skipped = %d, want 1", got)
	}

}

func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snapshot := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

func assertTreeUnchanged(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	after := snapshotTree(t, root)
	if len(after) != len(before) {
		t.Fatalf("read-only view changed file count: before=%v after=%v", keys(before), keys(after))
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok || !bytes.Equal(got, want) {
			t.Errorf("read-only view changed %s", path)
		}
	}
}

func keys(files map[string][]byte) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	return paths
}

// customStoredRecord runs a real custom parser and the shared stable formatter
// to create the canonical library record consumed by sfs archive views.
func customStoredRecord(t *testing.T, parser ulpengine.LineParser, formatter *ulpengine.StableFormatter, source string) string {
	t.Helper()
	host, url, login, password, ok := parser.Parse(source)
	if !ok {
		t.Fatalf("custom parser rejected %q", source)
	}
	line, ok := formatter.FormatRecordStable(host, url, login, password, false)
	if !ok {
		t.Fatalf("custom record is not stably representable: %q", source)
	}
	return line
}

// F10/F11: Stored archives use stored decoding, while raw -txt strict skips
// remain independent of the stored view. Match-all ensures malformed lines
// are actual hits whose skip count can be asserted.
func TestComboStoredFidelityModes(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "stored.zst")
	parser, err := ulpengine.NewDelimParser("|")
	if err != nil {
		t.Fatal(err)
	}
	formatter := ulpengine.NewStableFormatter()
	lines := []string{
		customStoredRecord(t, parser, formatter, "example.com|Zoë|pw"),
		customStoredRecord(t, parser, formatter, "example.com|kalai123$s|pw2"),
		customStoredRecord(t, parser, formatter, "example.com|user with space|pw3"),
		customStoredRecord(t, parser, formatter, "example.com:8080|12345|a:b:c"),
		"example.net:8443:fixed:pw:with:colon",
		"not a credential record",
		"final.example:final-user:tail",
	}
	content := strings.Join(lines, "\n") // final stored line intentionally has no newline
	writeZST(t, arch, []byte(content))
	if _, err := index.Build(context.Background(), arch, nil, nil); err != nil {
		t.Fatalf("index: %v", err)
	}
	// Complete the index's first digest verification before the read-only-view
	// snapshot; subsequent search must not backfill or rewrite its sidecar.
	if _, _, err := index.Ensure(context.Background(), arch, nil, nil); err != nil {
		t.Fatalf("ensure index: %v", err)
	}

	before := snapshotTree(t, dir)
	outputDir := t.TempDir()
	want := "Zoë:pw\nkalai123$s:pw2\nuser with space:pw3\n12345:a:b:c\nfixed:pw:with:colon\nfinal-user:tail\n"
	for _, mode := range []string{"stream", "file"} {
		t.Run(mode, func(t *testing.T) {
			var stream bytes.Buffer
			var skipped atomic.Int64
			metrics := &search.Metrics{}
			cfg := runConfig{
				root:            dir,
				pattern:         "*",
				matchAll:        true,
				archives:        []string{arch},
				workers:         1,
				stream:          mode == "stream",
				combo:           true,
				started:         time.Now(),
				metrics:         metrics,
				comboSkippedOut: &skipped,
			}
			if mode == "stream" {
				cfg.stdout = &stream
			} else {
				cfg.outFile = filepath.Join(outputDir, "combos.txt")
			}
			if err := run(context.Background(), cfg); err != nil {
				t.Fatalf("run: %v", err)
			}
			got := stream.String()
			if mode == "file" {
				data, err := os.ReadFile(cfg.outFile)
				if err != nil {
					t.Fatal(err)
				}
				got = string(data)
			}
			if got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
			if metrics.Hits.Load() != int64(len(lines)) {
				t.Errorf("matched hits = %d, want %d", metrics.Hits.Load(), len(lines))
			}
			if skipped.Load() != 1 {
				t.Errorf("combo skipped = %d, want 1", skipped.Load())
			}
		})
	}
	assertTreeUnchanged(t, dir, before)

}

// F11: raw -txt input stays strict in both output modes even though the same
// credential bytes are valid through stored archive decoding.
func TestComboRawTxtCompatibilityModes(t *testing.T) {
	content := "example.com:Zoë:pw\nexample.com:kalai123$s:pw2\nexample.com:normal:pw3\n"
	for _, mode := range []string{"stream", "file"} {
		t.Run(mode, func(t *testing.T) {
			cfg, outPath := comboTestConfig(t, content, true)
			var stream bytes.Buffer
			var skipped atomic.Int64
			cfg.comboSkippedOut = &skipped
			if mode == "stream" {
				cfg.outFile = ""
				cfg.stream = true
				cfg.stdout = &stream
			}
			if err := run(context.Background(), cfg); err != nil {
				t.Fatalf("run: %v", err)
			}
			got := stream.String()
			if mode == "file" {
				data, err := os.ReadFile(outPath)
				if err != nil {
					t.Fatal(err)
				}
				got = string(data)
			}
			if got != "normal:pw3\n" {
				t.Errorf("output = %q, want %q", got, "normal:pw3\\n")
			}
			if cfg.metrics.Hits.Load() != 3 {
				t.Errorf("matched hits = %d, want 3", cfg.metrics.Hits.Load())
			}
			if skipped.Load() != 2 {
				t.Errorf("combo skipped = %d, want 2", skipped.Load())
			}
		})
	}
}
