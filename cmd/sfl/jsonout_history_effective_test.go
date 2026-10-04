package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H-02: the validator must compare the -json target against the EFFECTIVE
// history endpoints. An existing directory -history-path resolves to
// DIR/history.sqlite3 (plus -wal/-shm sidecars) when the store opens — the
// raw spelling never matches the target, so the stream truncated the database
// while validation stayed silent. The direct file spelling keeps working.
func TestValidateJSONOutTargetRejectsEffectiveHistoryPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		targetPath func(histDir string) string
	}{
		{"resolved database", func(histDir string) string {
			return filepath.Join(histDir, "history.sqlite3")
		}},
		{"resolved WAL sidecar", func(histDir string) string {
			return filepath.Join(histDir, "history.sqlite3") + "-wal"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "in.txt")
			if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			histDir := filepath.Join(dir, "hist")
			if err := os.MkdirAll(histDir, 0o700); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(histDir, "history.sqlite3")
			content := []byte("seeded database content 0123456789abcdef")
			if err := os.WriteFile(db, content, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := runConfig{
				Input: input, OutputDir: filepath.Join(dir, "out"),
				RunStamp: "20260927_AAAAAA", History: true, HistoryPath: histDir,
				JSONOut: tc.targetPath(histDir),
			}
			err := validateJSONOutTarget(cfg)
			if err == nil {
				t.Fatalf("directory -history-path with -json at %s must be refused", cfg.JSONOut)
			}
			if !strings.Contains(err.Error(), "overlaps history database") {
				t.Fatalf("wrong reason: %v", err)
			}
			got, rerr := os.ReadFile(db)
			if rerr != nil || !bytes.Equal(got, content) {
				t.Fatalf("validation must not touch the database: %q (err %v)", got, rerr)
			}
		})
	}
}

// A directory -history-path with a stream target outside the effective
// database endpoints is still allowed.
func TestValidateJSONOutTargetAllowsDirHistoryWithForeignTarget(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(input, []byte("https://example.com:user:pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	histDir := filepath.Join(dir, "hist")
	if err := os.MkdirAll(histDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig{
		Input: input, OutputDir: filepath.Join(dir, "out"),
		RunStamp: "20260927_AAAAAA", History: true, HistoryPath: histDir,
		JSONOut: filepath.Join(dir, "stats.jsonl"),
	}
	if err := validateJSONOutTarget(cfg); err != nil {
		t.Fatalf("non-colliding target must be allowed: %v", err)
	}
}
