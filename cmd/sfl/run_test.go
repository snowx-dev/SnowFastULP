package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		Started: time.Date(2026, 6, 26, 21, 0, 0, 0, time.UTC),
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
		Started: time.Date(2026, 6, 26, 21, 1, 0, 0, time.UTC),
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
