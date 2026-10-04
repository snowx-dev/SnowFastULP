package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H-01: the stream truncates its target before the history store opens, so a
// -json target naming the effective history database (or its WAL/SHM
// sidecars) would destroy the shared DB underneath the run. The collision
// must be refused before anything is opened, in every -history-path spelling
// — including the directory form, which the store resolves to
// DIR/history.sqlite3 only at open time.
func TestJSONOutTargetCollidingWithHistoryDatabaseRefused(t *testing.T) {
	for _, tc := range []struct {
		name       string
		historyDir bool
		fileExists bool // whether the target exists after the seed run
		target     func(dir string) (raw string, targetPath string)
	}{
		{
			name:       "direct database file",
			historyDir: false,
			fileExists: true,
			target: func(dir string) (string, string) {
				db := filepath.Join(dir, "history.sqlite3")
				return db, db
			},
		},
		{
			name:       "directory spelling resolves to the database",
			historyDir: true,
			fileExists: true,
			target: func(dir string) (string, string) {
				histDir := filepath.Join(dir, "hist")
				return histDir, filepath.Join(histDir, "history.sqlite3")
			},
		},
		{
			name:       "effective WAL sidecar",
			historyDir: true,
			fileExists: false,
			target: func(dir string) (string, string) {
				histDir := filepath.Join(dir, "hist")
				return histDir, filepath.Join(histDir, "history.sqlite3") + "-wal"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input := sfuSpaceFormInput(t, dir)
			out1 := filepath.Join(dir, "out1.d")
			out2 := filepath.Join(dir, "out2.d")
			if err := os.MkdirAll(out1, 0o755); err != nil {
				t.Fatal(err)
			}
			raw, targetPath := tc.target(dir)
			// The directory spelling only adopts the in-dir database name when
			// the directory exists (or carries a trailing separator); a
			// missing path would be a plain database file.
			if tc.historyDir {
				if err := os.MkdirAll(raw, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			// Seed a real history database at the effective path.
			stderr, code := runSFUSpaceForm(t, dir, "",
				"-no-tui", "-no-update-check", "-history", "-history-path", raw,
				"-o", out1+string(filepath.Separator), input)
			if code != 0 {
				t.Fatalf("seed run: exit = %d, want 0\nstderr:\n%s", code, stderr)
			}
			var before []byte
			if tc.fileExists {
				var err error
				before, err = os.ReadFile(targetPath)
				if err != nil {
					t.Fatalf("seed run did not create %s: %v", targetPath, err)
				}
				if !bytes.HasPrefix(before, []byte("SQLite format 3")) {
					t.Fatalf("seeded %s is not a SQLite database", targetPath)
				}
			}

			if err := os.MkdirAll(out2, 0o755); err != nil {
				t.Fatal(err)
			}
			stderr, code = runSFUSpaceForm(t, dir, "",
				"-no-tui", "-no-update-check", "-history", "-history-path", raw,
				"-json", targetPath,
				"-o", out2+string(filepath.Separator), input)
			if code != 2 {
				t.Fatalf("%s: exit = %d, want 2\nstderr:\n%s", tc.name, code, stderr)
			}
			if !strings.Contains(stderr, "overlaps history database") {
				t.Fatalf("%s: stderr missing the collision reason:\n%s", tc.name, stderr)
			}
			after, err := os.ReadFile(targetPath)
			if tc.fileExists {
				if err != nil {
					t.Fatalf("refused run destroyed %s: %v", targetPath, err)
				}
				if !bytes.Equal(before, after) {
					t.Fatalf("%s: history database truncated: %d -> %d bytes", tc.name, len(before), len(after))
				}
			} else if err == nil {
				t.Fatalf("%s: refused run created %s", tc.name, targetPath)
			}
		})
	}
}

// Without -history there is no database to protect; a target merely named
// like one must keep working.
func TestJSONOutTargetSameNameWithoutHistoryAllowed(t *testing.T) {
	dir := t.TempDir()
	input := sfuSpaceFormInput(t, dir)
	out := filepath.Join(dir, "out.d")
	jsonl := filepath.Join(dir, "history.sqlite3")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-json", jsonl,
		"-o", out+string(filepath.Separator), input)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
}
