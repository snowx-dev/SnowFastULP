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

func TestIsPasswordFileCoversNonRedLineFamilies(t *testing.T) {
	accept := []string{
		"passwords.txt",
		"All Passwords.txt",
		"pws.txt",                   // Raccoon
		"Logins_Chrome_Default.txt", // HESOYAM flat
		"Mozilla Firefox_ab12cd34.default-release_logins.txt",              // Firefox export
		filepath.Join("Browser", "Logins", "Chrome_Default[d70b625c].txt"), // Rhadamanthys dir
	}
	for _, name := range accept {
		if !isPasswordFile(filepath.Join("/root/log", name)) {
			t.Errorf("expected %q to be treated as a credential file", name)
		}
	}

	reject := []string{
		"Cookies_Chrome_Default.txt",
		"Autofills_Chrome_Default.txt",
		"System.txt",
		"passwordcracker.txt",
		"Chrome.txt", // bare browser name is ambiguous, intentionally skipped
		filepath.Join("Cookies", "Chrome_Default.txt"),
	}
	for _, name := range reject {
		if isPasswordFile(filepath.Join("/root/log", name)) {
			t.Errorf("expected %q NOT to be treated as a credential file", name)
		}
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
