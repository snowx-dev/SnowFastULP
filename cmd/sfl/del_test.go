package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunDeletesParsedTopLevelSubfolders(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	victimA := filepath.Join(input, "victimA")
	victimB := filepath.Join(input, "victimB")
	writeFile(t, filepath.Join(victimA, "All Passwords.txt"), "URL: a.com\nUSER: u\nPASS: p\n")
	writeFile(t, filepath.Join(victimB, "Passwords.txt"), "URL: b.com\nUSER: u2\nPASS: p2\n")
	outDir := filepath.Join(dir, "out")

	if err := run(runConfig{
		Input: input, OutputDir: outDir, Workers: 2, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 10, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(victimA); !os.IsNotExist(err) {
		t.Fatalf("victimA should be deleted: %v", err)
	}
	if _, err := os.Stat(victimB); !os.IsNotExist(err) {
		t.Fatalf("victimB should be deleted: %v", err)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatalf("input root must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "sfl_20260626_211000.txt")); err != nil {
		t.Fatalf("output must survive: %v", err)
	}
}

func TestRunDelRetainsFailedArchiveButDeletesGoodSubfolder(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "logs")
	victim := filepath.Join(input, "victim")
	writeFile(t, filepath.Join(victim, "Passwords.txt"), "URL: a.com\nUSER: u\nPASS: p\n")
	badZip := filepath.Join(input, "locked.zip")
	writeEncryptedRunZip(t, badZip, "ice", "victim/Passwords.txt", "URL: z.com\nUSER: u\nPASS: p\n")

	pwPath := filepath.Join(dir, "pw.txt")
	writeFile(t, pwPath, "wrong\n")
	outDir := filepath.Join(dir, "out")

	if err := run(runConfig{
		Input: input, OutputDir: outDir, Password: pwPath, Workers: 2, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 11, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatalf("good victim should be deleted: %v", err)
	}
	if _, err := os.Stat(badZip); err != nil {
		t.Fatalf("bad-password archive must be retained: %v", err)
	}
}

// A nested archive whose password is missing fails in isolation: the outer
// archive parses OK but is flagged HadIssue, so -del must retain it rather than
// discard the un-cracked inner data.
func TestRunDelRetainsArchiveWithUncrackedNested(t *testing.T) {
	dir := t.TempDir()

	innerPath := filepath.Join(dir, "inner.zip")
	writeEncryptedRunZip(t, innerPath, "ice", "victim/Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")
	innerBytes, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(innerPath); err != nil {
		t.Fatal(err)
	}

	outer := filepath.Join(dir, "outer.zip")
	f, err := os.Create(outer)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("v.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(innerBytes); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	pwPath := filepath.Join(dir, "pw.txt")
	writeFile(t, pwPath, "wrong\n") // lacks "ice"
	outDir := filepath.Join(dir, "out")

	if err := run(runConfig{
		Input: outer, OutputDir: outDir, Password: pwPath, Workers: 1, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 13, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(outer); err != nil {
		t.Fatalf("outer archive with an uncracked nested member must be retained: %v", err)
	}
}

func TestRunDelRemovesSingleArchiveInput(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "logs.zip")
	writeEncryptedRunZip(t, archivePath, "ice", "victim/Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")
	pwPath := filepath.Join(dir, "pw.txt")
	writeFile(t, pwPath, "ice\n")
	outDir := filepath.Join(dir, "out")

	if err := run(runConfig{
		Input: archivePath, OutputDir: outDir, Password: pwPath, Workers: 1, NoTUI: true, DeleteSources: true,
		Started: time.Date(2026, 6, 26, 21, 12, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("single archive input should be deleted: %v", err)
	}
}
