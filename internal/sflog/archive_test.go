package sflog

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	zipenc "github.com/yeka/zip"
)

func TestExtractPathToWriterStreamsZipMembers(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "logs.zip")
	writeTestZip(t, archivePath, map[string]string{
		"victim/Passwords.txt": "URL: https://zip.example.com/login\nUSER: zipped\nPASS: secret\n",
		"victim/System.txt":    "OS: Windows\n",
	})

	var out bytes.Buffer
	stats, err := ExtractPathToWriter(archivePath, &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchivesScanned != 1 || stats.FilesScanned != 1 || stats.Emitted != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := out.String(); got != "zip.example.com/login:zipped:secret\n" {
		t.Fatalf("out = %q", got)
	}
}

func TestLoadPasswordsTreatsExistingFileAsWordlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwords.txt")
	if err := os.WriteFile(path, []byte("\nfirst\n second \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	passwords, err := LoadPasswords(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"", "first", "second"}
	if len(passwords) != len(want) {
		t.Fatalf("passwords = %#v", passwords)
	}
	for i := range want {
		if passwords[i] != want[i] {
			t.Fatalf("passwords = %#v want %#v", passwords, want)
		}
	}
}

func TestExtractPathToWriterUsesPasswordCandidatesForEncryptedZip(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "secret.zip")
	writeEncryptedTestZip(t, archivePath, "ice", "victim/Passwords.txt", "URL: https://secret.example.com\nUSER: analyst\nPASS: pw\n")

	var out bytes.Buffer
	stats, err := ExtractPathToWriterWithPasswords(archivePath, &out, false, []string{"", "wrong", "ice"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchivesScanned != 1 || stats.SkippedArchives != 0 || stats.Emitted != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := out.String(); got != "secret.example.com:analyst:pw\n" {
		t.Fatalf("out = %q", got)
	}
}

func TestExtractPathToWriterReadsLegacyZipCrypto(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "legacy.zip")
	writeEncryptedTestZipMethod(t, archivePath, "ice", "victim/Passwords.txt",
		"URL: https://legacy.example.com\nUSER: analyst\nPASS: pw\n", zipenc.StandardEncryption)

	var out bytes.Buffer
	stats, err := ExtractPathToWriterWithPasswords(archivePath, &out, false, []string{"", "wrong", "ice"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchivesScanned != 1 || stats.SkippedArchives != 0 || stats.Emitted != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := out.String(); got != "legacy.example.com:analyst:pw\n" {
		t.Fatalf("out = %q", got)
	}
}

func TestLoadPasswordsTreatsMissingPathAsLiteral(t *testing.T) {
	passwords, err := LoadPasswords("not-a-file-secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(passwords) != 2 || passwords[0] != "" || passwords[1] != "not-a-file-secret" {
		t.Fatalf("passwords = %#v", passwords)
	}
}

func writeTestZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeEncryptedTestZip(t *testing.T, path, password, name, body string) {
	t.Helper()
	writeEncryptedTestZipMethod(t, path, password, name, body, zipenc.AES256Encryption)
}

func writeEncryptedTestZipMethod(t *testing.T, path, password, name, body string, method zipenc.EncryptionMethod) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zipenc.NewWriter(f)
	w, err := zw.Encrypt(name, password, method)
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
