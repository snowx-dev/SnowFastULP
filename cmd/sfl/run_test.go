package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
	zipenc "github.com/yeka/zip"
)

func TestRunWritesClassicULPOutput(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"), []byte("URL: https://a.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	if err := run(runConfig{
		Input: input, OutputDir: outDir, Workers: 1, NoTUI: true,
		RunStamp: "20260626_210000",
	}); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(outDir, "sfl_20260626_210000.txt")
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a.example.com/login:u:p\n" {
		t.Fatalf("output = %q", string(got))
	}
}

func TestRunUsesPasswordFileForEncryptedZip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "logs.zip")
	writeEncryptedRunZip(t, archivePath, "ice", "victim/Passwords.txt", "URL: https://zip.example.com\nUSER: u\nPASS: p\n")
	pwPath := filepath.Join(dir, "passwords.txt")
	if err := os.WriteFile(pwPath, []byte("wrong\nice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	if err := run(runConfig{
		Input: archivePath, OutputDir: outDir, Password: pwPath, Workers: 1, NoTUI: true,
		RunStamp: "20260626_210100",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "sfl_20260626_210100.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "zip.example.com:u:p\n" {
		t.Fatalf("output = %q", string(got))
	}
}

// A -od run that extracts nothing must leave the library untouched, rather
// than erroring out. Under the exit-code policy it returns *runOutcome 4
// (nothing usable — the only source had no credentials) instead of a hard
// error; the library-untouched guarantee below is the real assertion.
func TestRunODEmptyLeavesLibraryUnchanged(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	// Recognized password file, but no parseable credentials in it.
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("Browser: Chrome\nProfile: Default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := run(runConfig{
		Input: filepath.Join(dir, "logs"), LibraryDir: lib, Workers: 1, NoTUI: true, NoUpdateCheck: true,
		Started: time.Date(2026, 6, 26, 21, 5, 0, 0, time.UTC),
	}); err != nil {
		var outcome *runOutcome
		if !errors.As(err, &outcome) || outcome.code != exitcode.NothingUsable {
			t.Fatalf("empty -od run err = %v, want runOutcome 4 or nil", err)
		}
	}

	entries, err := os.ReadDir(lib)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zst") {
			t.Fatalf("library must be unchanged, found new archive %q", e.Name())
		}
	}
}

// The -od archive and the -env secrets dir must share the run's one stamp:
// sfu_<stamp>.txt.zst lands in the library while sfl_<stamp>_secrets sits
// beside it, both named from the same cfg.RunStamp.
func TestRunIngestSharesRunStampWithEnvDir(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "All Passwords.txt"), []byte("URL: https://a.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An env candidate so the lazily created secrets dir actually exists.
	if err := os.WriteFile(filepath.Join(input, "config.env"), []byte("API_KEY=example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "library")
	const stamp = "20260920_test04"
	if err := run(runConfig{
		Input: filepath.Join(dir, "logs"), LibraryDir: lib, Workers: 1, Env: true,
		NoTUI: true, NoUpdateCheck: true, RunStamp: stamp,
	}); err != nil {
		t.Fatal(err)
	}
	envDir := filepath.Join(lib, "sfl_"+stamp+"_secrets")
	if fi, err := os.Stat(envDir); err != nil || !fi.IsDir() {
		t.Fatalf("env dir %s missing or not a dir: %v", envDir, err)
	}
	archives, err := filepath.Glob(filepath.Join(lib, "sfu_"+stamp+"*.txt.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) == 0 {
		entries, _ := os.ReadDir(lib)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no sfu_%s archive in library; entries = %v", stamp, names)
	}
}

func writeEncryptedRunZip(t *testing.T, path, password, name, body string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zipenc.NewWriter(f)
	w, err := zw.Encrypt(name, password, zipenc.AES256Encryption)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
