package sflog

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// --- nested (archive-in-archive) recursion ---------------------------------

// zipBytes builds an in-memory zip whose members are raw bytes, so an inner
// archive can be embedded verbatim as a member of an outer archive.
func zipBytes(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeZipMembers(t *testing.T, path string, members map[string][]byte) {
	t.Helper()
	if err := os.WriteFile(path, zipBytes(t, members), 0o644); err != nil {
		t.Fatal(err)
	}
}

// encryptedZipBytes returns the bytes of an encrypted zip (single member).
func encryptedZipBytes(t *testing.T, password, name, body string) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "enc.zip")
	writeEncryptedTestZip(t, p, password, name, body)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNestedZipInZipExtracted(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://inner.example.com/login\nUSER: nested\nPASS: deep\n"),
	})
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{"victims/v1.zip": inner})

	var out bytes.Buffer
	stats, err := ExtractPathToWriter(outer, &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchivesScanned != 2 || stats.FilesScanned != 1 || stats.Emitted != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := out.String(); got != "inner.example.com/login:nested:deep\n" {
		t.Fatalf("out = %q", got)
	}
}

func TestNestedEncryptedZipExtractedWithPassword(t *testing.T) {
	inner := encryptedZipBytes(t, "ice", "victim/Passwords.txt",
		"URL: https://enc.example.com\nUSER: a\nPASS: p\n")
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{"v.zip": inner})

	var out bytes.Buffer
	stats, err := ExtractPathToWriterWithPasswords(outer, &out, false, []string{"", "wrong", "ice"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Emitted != 1 || stats.SkippedArchives != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if got := out.String(); got != "enc.example.com:a:p\n" {
		t.Fatalf("out = %q", got)
	}
}

func TestNestedBadPasswordIsolatedNonFatal(t *testing.T) {
	inner := encryptedZipBytes(t, "ice", "victim/Passwords.txt",
		"URL: https://locked.example.com\nUSER: a\nPASS: p\n")
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{
		"loose/Passwords.txt": []byte("URL: https://loose.example.com\nUSER: u\nPASS: pw\n"),
		"v.zip":               inner,
	})

	var out bytes.Buffer
	// Wordlist deliberately lacks "ice": the inner archive must fail in
	// isolation while the outer's loose credential still extracts.
	stats, err := ExtractPathToWriterWithPasswords(outer, &out, false, []string{"", "wrong"})
	if err != nil {
		t.Fatalf("run must not abort: %v", err)
	}
	if stats.Emitted != 1 || stats.SkippedArchives != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.PasswordNotFound < 1 {
		t.Fatalf("expected a password-not-found issue, stats = %+v", stats)
	}
	var sawNested bool
	for _, is := range stats.Issues {
		if is.Kind == IssuePasswordNotFound && strings.Contains(is.Path, "!v.zip") {
			sawNested = true
		}
	}
	if !sawNested {
		t.Fatalf("missing nested password issue with provenance, issues = %+v", stats.Issues)
	}
	if got := out.String(); got != "loose.example.com:u:pw\n" {
		t.Fatalf("out = %q", got)
	}
}

func TestNestedMixedLooseAndNested(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://nested.example.com\nUSER: n\nPASS: np\n"),
	})
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{
		"loose/Passwords.txt": []byte("URL: https://loose.example.com\nUSER: l\nPASS: lp\n"),
		"bundle.zip":          inner,
	})

	var out bytes.Buffer
	stats, err := ExtractPathToWriter(outer, &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchivesScanned != 2 || stats.FilesScanned != 2 || stats.Emitted != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("out = %q", out.String())
	}
}

func TestNestedDepthCapSkipsTooDeep(t *testing.T) {
	// 5 archive levels: outer -> l1 -> l2 -> l3 -> l4(creds). The innermost sits
	// beyond maxArchiveDepth (3) and must be skipped with an isolated issue.
	l4 := zipBytes(t, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://toodeep.example.com\nUSER: x\nPASS: y\n"),
	})
	l3 := zipBytes(t, map[string][]byte{"l4.zip": l4})
	l2 := zipBytes(t, map[string][]byte{"l3.zip": l3})
	l1 := zipBytes(t, map[string][]byte{"l2.zip": l2})
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{"l1.zip": l1})

	var out bytes.Buffer
	stats, err := ExtractPathToWriter(outer, &out, false)
	if err != nil {
		t.Fatalf("depth cap must not abort: %v", err)
	}
	if stats.Emitted != 0 {
		t.Fatalf("expected nothing past the depth cap, out = %q stats = %+v", out.String(), stats)
	}
	var sawTooDeep bool
	for _, is := range stats.Issues {
		if is.Err != nil && strings.Contains(is.Err.Error(), "nesting too deep") {
			sawTooDeep = true
		}
	}
	if !sawTooDeep {
		t.Fatalf("missing depth-cap issue, issues = %+v", stats.Issues)
	}
}

func TestNestedSevenZipContainingZip(t *testing.T) {
	bin := sevenZipBinary()
	if bin == "" {
		t.Skip("no 7z binary available to build a .7z fixture")
	}
	dir := t.TempDir()
	innerPath := filepath.Join(dir, "inner.zip")
	writeZipMembers(t, innerPath, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://sevenz.example.com/login\nUSER: a\nPASS: p\n"),
	})
	outer := filepath.Join(dir, "outer.7z")
	cmd := exec.Command(bin, "a", "-y", "-bso0", "-bsp0", outer, "inner.zip")
	cmd.Dir = dir
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("7z create failed: %v\n%s", err, combined)
	}

	var out bytes.Buffer
	stats, err := ExtractPathToWriter(outer, &out, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ArchivesScanned != 2 || stats.Emitted != 1 {
		t.Fatalf("stats = %+v out = %q", stats, out.String())
	}
	if got := out.String(); got != "sevenz.example.com/login:a:p\n" {
		t.Fatalf("out = %q", got)
	}
}

func sevenZipBinary() string {
	for _, b := range []string{"7z", "7za", "7zr"} {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	return ""
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
