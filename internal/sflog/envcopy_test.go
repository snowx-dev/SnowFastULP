package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsEnvCopyCandidate(t *testing.T) {
	positive := []string{
		".env", ".env.local", "foo.env", "id_rsa", "secrets.json",
		"credentials.json", "wallet.dat", "config.pem", "key.key",
		"my-apikeys.json", "appsettings.Development.json",
	}
	for _, p := range positive {
		if !isEnvCopyCandidate(p) {
			t.Errorf("%q should be an env copy candidate", p)
		}
	}
	negative := []string{
		"id_rsa.pub", "id_ed25519.pub", "config", "Passwords.txt",
		"readme.md", "package.json", "image.png", "app.py", "debug.log",
	}
	for _, p := range negative {
		if isEnvCopyCandidate(p) {
			t.Errorf("%q should not be an env copy candidate", p)
		}
	}
}

func TestSafeRelPath(t *testing.T) {
	got := safeRelPath(`../evil/../../.env`)
	if got == ".." || got == "" {
		t.Fatalf("safeRelPath = %q, want sanitized path", got)
	}
	if got := safeRelPath("VictimA/deep/.env"); got != filepath.Join("VictimA", "deep", ".env") {
		t.Fatalf("safeRelPath nested = %q, want VictimA/deep/.env", got)
	}
}

func TestEnvCopyLooseFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "VictimA")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, victim, "config.env", "API_KEY=secret\n")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied < 1 {
		t.Fatalf("copied = %d, want >= 1", es.Copied)
	}
	if _, err := os.Stat(filepath.Join(root, "config.env")); err != nil {
		t.Fatalf("env file not copied flat: %v", err)
	}
}

func TestEnvCopyFlatArchive(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/secrets.json")
	_, _ = w.Write([]byte(`{"key":"val"}`))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 1 {
		t.Fatalf("copied = %d, want 1", es.Copied)
	}
	// Flat: the in-archive path "VictimA/secrets.json" collapses to basename.
	if _, err := os.Stat(filepath.Join(root, "secrets.json")); err != nil {
		t.Fatalf("env file not copied flat: %v", err)
	}
}

func TestEnvCopyFlatCollision(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("deep/.env")
	_, _ = w.Write([]byte("first\n"))
	w2, _ := zw.Create("other/.env")
	_, _ = w2.Write([]byte("second\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 2 {
		t.Fatalf("copied = %d, want 2", es.Copied)
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		t.Fatalf("first .env missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".env_2")); err != nil {
		t.Fatalf("collided .env_2 missing: %v", err)
	}
}

func TestEnqueueFileSkipsOverCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.env")
	if err := os.WriteFile(path, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	copier := NewEnvCopier(t.TempDir(), nil, 512)
	copier.Start()
	copier.EnqueueFile(path)
	es := copier.Close()
	if es.SkippedOverCap != 1 {
		t.Fatalf("SkippedOverCap = %d, want 1", es.SkippedOverCap)
	}
}

func TestCopyFileSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	dest := filepath.Join(dir, "out.env")
	if err := copyFile(link, dest); err == nil {
		t.Fatal("expected error copying symlink")
	}
}

func writeTestFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
