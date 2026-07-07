package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// TestRunODIngestsGeneratedULP exercises the in-process -od path end to end:
// sfl extracts the victim log, then merges the generated ULP into the library
// via ulpengine (no sfu subprocess), producing a single sfu_*.txt.zst archive
// whose contents match the extracted credential.
func TestRunODIngestsGeneratedULP(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"),
		[]byte("URL: https://od.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	libDir := filepath.Join(dir, "library")

	if err := run(runConfig{
		Input: input, LibraryDir: libDir, Workers: 1, NoTUI: true, NoUpdateCheck: true,
		Started: time.Date(2026, 6, 26, 21, 2, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	archives, err := filepath.Glob(filepath.Join(libDir, "sfu_*.txt.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives = %v, want exactly one", archives)
	}
	if got := readZst(t, archives[0]); got != "od.example.com/login:u:p\n" {
		t.Fatalf("library archive = %q", got)
	}

	sidecars, err := filepath.Glob(filepath.Join(libDir, "sfu_dedup_idx", "*.idx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sidecars) != 1 {
		t.Fatalf("sidecars = %v, want exactly one", sidecars)
	}

	// Cross-boundary cleanup: a successful in-process ingest must leave no engine
	// scratch (.sfu-tmp-*) in the library nor any sfl decrypted-ULP workdir
	// (sfl-od-*) in its parent.
	assertNoLeftovers(t, filepath.Join(libDir, ".sfu-tmp-*"))
	assertNoLeftovers(t, filepath.Join(dir, "sfl-od-*"))
}

func assertNoLeftovers(t *testing.T, pattern string) {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leftover temp dirs for %q: %v", pattern, matches)
	}
}

func readZst(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	b, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
