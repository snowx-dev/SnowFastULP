package sflog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverPasswordFilesHandlesSingleAndBatchFolders(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		filepath.Join(root, "victim-one", "All Passwords.txt"),
		filepath.Join(root, "victim-two", "Browsers", "Passwords_Chrome.txt"),
		filepath.Join(root, "victim-two", "System.txt"),
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("URL: https://example.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files, err := DiscoverPasswordFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files: %+v", len(files), files)
	}
	if files[0].Path != paths[0] || files[1].Path != paths[1] {
		t.Fatalf("files = %+v", files)
	}
}

func TestDiscoverPasswordFilesAcceptsLogExtension(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "victim", "passwords.log")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("URL: https://example.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := DiscoverPasswordFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != p {
		t.Fatalf("files = %+v", files)
	}
}

func TestDiscoverPasswordFilesAcceptsSinglePasswordFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "passwords.txt")
	if err := os.WriteFile(p, []byte("URL: https://example.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := DiscoverPasswordFiles(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != p {
		t.Fatalf("files = %+v", files)
	}
}
