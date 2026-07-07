package sflog

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bodgit/sevenzip"
	zipenc "github.com/yeka/zip"
)

// splitBlob writes blob into dir as "<logical>.001", ".002", ... in n roughly
// equal parts and returns the ordered part paths, reproducing what 7z -v / the
// `split` tool produce for a single archive.
func splitBlob(t *testing.T, dir, logical string, blob []byte, n int) []string {
	t.Helper()
	if n < 1 {
		n = 1
	}
	chunk := (len(blob) + n - 1) / n
	var parts []string
	for i := 0; ; i++ {
		start := i * chunk
		end := start + chunk
		if end > len(blob) {
			end = len(blob)
		}
		p := filepath.Join(dir, fmt.Sprintf("%s.%03d", logical, i+1))
		if err := os.WriteFile(p, blob[start:end], 0o644); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p)
		if end >= len(blob) {
			break
		}
	}
	return parts
}

// collectCtx returns an extractCtx that gathers emitted creds and captures debug
// lines, with no Progress (creditor is nil-safe).
func collectCtx(t *testing.T, passwords []string, display string) (*extractCtx, *[]Credential, *[]string) {
	t.Helper()
	var creds []Credential
	var dbg []string
	ec := extractCtx{
		passwords: passwords,
		tempDir:   t.TempDir(),
		display:   display,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue:   func(string, IssueKind, error) {},
		debug:     func(f string, a ...any) { dbg = append(dbg, fmt.Sprintf(f, a...)) },
	}
	return &ec, &creds, &dbg
}

func TestReadSplitArchivePlainZip(t *testing.T) {
	blob := zipBytes(t, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://split.example/login\nUSER: a\nPASS: p\n"),
	})
	dir := t.TempDir()
	parts := splitBlob(t, dir, "data.zip", blob, 3)
	if len(parts) < 2 {
		t.Fatalf("expected a multi-part split, got %d parts", len(parts))
	}

	ec, creds, dbg := collectCtx(t, []string{""}, parts[0])
	scan, err := readSplitArchive(context.Background(), parts, *ec, 300)
	if err != nil {
		t.Fatalf("readSplitArchive: %v", err)
	}
	if scan.files != 1 || len(*creds) != 1 {
		t.Fatalf("scan=%+v creds=%v, want 1 file / 1 cred", scan, *creds)
	}
	if !strings.Contains((*creds)[0].URL, "split.example") {
		t.Fatalf("cred URL = %q", (*creds)[0].URL)
	}
	if !anyContains(*dbg, "opening split set (3 parts, .zip") {
		t.Fatalf("missing split-set debug line: %v", *dbg)
	}
}

func TestReadSplitArchiveEncryptedZip(t *testing.T) {
	blob := encryptedZipBytes(t, "ice", "victim/Passwords.txt",
		"URL: https://enc-split.example\nUSER: a\nPASS: p\n")
	dir := t.TempDir()
	parts := splitBlob(t, dir, "enc.zip", blob, 2)

	ec, creds, _ := collectCtx(t, []string{"", "wrong", "ice"}, parts[0])
	if _, err := readSplitArchive(context.Background(), parts, *ec, 200); err != nil {
		t.Fatalf("readSplitArchive: %v", err)
	}
	if len(*creds) != 1 {
		t.Fatalf("encrypted split creds = %v, want 1", *creds)
	}
}

func TestReadSplitArchiveNestedZip(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://nested-in-split.example\nUSER: a\nPASS: p\n"),
	})
	outer := zipBytes(t, map[string][]byte{"nest/inner.zip": inner})
	dir := t.TempDir()
	parts := splitBlob(t, dir, "outer.zip", outer, 2)

	ec, creds, _ := collectCtx(t, []string{""}, parts[0])
	scan, err := readSplitArchive(context.Background(), parts, *ec, 200)
	if err != nil {
		t.Fatalf("readSplitArchive: %v", err)
	}
	if scan.nestedArchives < 1 {
		t.Fatalf("nestedArchives = %d, want >= 1", scan.nestedArchives)
	}
	if len(*creds) != 1 || !strings.Contains((*creds)[0].URL, "nested-in-split.example") {
		t.Fatalf("nested split creds = %v", *creds)
	}
}

// TestReadZipFilesSmallestProbe verifies the probe is the smallest encrypted
// member, so StageTestingPassword stays a short blip on large sets.
func TestReadZipFilesSmallestProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "two.zip")
	writeEncryptedZipMembers(t, path, "ice", map[string]string{
		"victim/Passwords.txt":     "URL: https://small.example\nUSER: a\nPASS: p\n",
		"victim/All Passwords.txt": "URL: https://big.example\nUSER: a\nPASS: p\n" + strings.Repeat("# pad\n", 2000),
	})
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	ec, creds, dbg := collectCtx(t, []string{"ice"}, path)
	if _, err := readZipFiles(context.Background(), zr.File, *ec, 1000); err != nil {
		t.Fatalf("readZipFiles: %v", err)
	}
	if len(*creds) != 2 {
		t.Fatalf("creds = %d, want 2 (both members parsed)", len(*creds))
	}
	// The probe debug line must name the SMALL member, not the large one.
	var probeLine string
	for _, l := range *dbg {
		if strings.Contains(l, "probe member ") {
			probeLine = l
		}
	}
	if !strings.Contains(probeLine, "probe member Passwords.txt (") {
		t.Fatalf("probe line = %q, want the small 'Passwords.txt' member", probeLine)
	}
}

// TestReadSplitArchive7z builds a split 7z with the system 7z packer (skipped
// when absent) and proves the concatenated reader extracts it.
func TestReadSplitArchive7z(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found; skipping split-7z test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "Passwords.txt")
	body := "URL: https://split7z.example\nUSER: a\nPASS: p\n" + strings.Repeat("# pad line\n", 4000)
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Store mode (-mx=0) keeps the archive larger than the volume size so -v8k
	// actually splits it (compressed pad lines would otherwise fit in one part).
	cmd := exec.Command(bin, "a", "-mx=0", "-v8k", "out.7z", "Passwords.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("7z pack failed (%v): %s", err, out)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "out.7z.*"))
	sort.Strings(matches)
	if len(matches) < 2 {
		t.Skipf("7z produced %d part(s), need a real split", len(matches))
	}

	ec, creds, dbg := collectCtx(t, []string{""}, matches[0])
	scan, err := readSplitArchive(context.Background(), matches, *ec, 1000)
	if err != nil {
		t.Fatalf("readSplitArchive(7z): %v", err)
	}
	if scan.files != 1 || len(*creds) != 1 || !strings.Contains((*creds)[0].URL, "split7z.example") {
		t.Fatalf("7z split scan=%+v creds=%v", scan, *creds)
	}
	if !anyContains(*dbg, "opening split set") || !anyContains(*dbg, ".7z") {
		t.Fatalf("missing 7z split debug line: %v", *dbg)
	}
}

// TestReadSevenZipMembersCreditsBytes proves a 7z credits on-disk bytes as its
// credential members are read (so the TUI bar moves) instead of staying at 0
// until finish() jumps to 100%. It drives readSevenZipMembers directly so no
// finish() top-up can mask the per-member crediting.
func TestReadSevenZipMembersCreditsBytes(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found; skipping 7z crediting test")
	}
	dir := t.TempDir()
	var names []string
	for i := 0; i < 6; i++ {
		n := fmt.Sprintf("Passwords-%d.txt", i)
		names = append(names, n)
		body := fmt.Sprintf("URL: https://z%d.example\nUSER: a\nPASS: p\n", i) + strings.Repeat("# pad line\n", 3000)
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Default compression (uncompressed >> on-disk) exercises the scaleFor
	// mapping: crediting raw decoded bytes 1:1 would overshoot, so the scale must
	// map them back onto the on-disk weight.
	cmd := exec.Command(bin, append([]string{"a", "-y", "-bso0", "-bsp0", "out.7z"}, names...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("7z create failed: %v\n%s", err, out)
	}
	path := filepath.Join(dir, "out.7z")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	weight := fi.Size()

	rc, err := sevenzip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	prog := &Progress{}
	prog.total.Store(weight)
	cr := newCreditor(prog, weight, 1)
	creds := 0
	ec := extractCtx{
		display: path,
		p:       prog,
		emit:    func(Credential) { creds++ },
		onIssue: func(string, IssueKind, error) {},
	}

	// No cr.finish(): whatever is credited is purely per-member.
	scan, had, err := readSevenZipMembers(context.Background(), ec, &rc.Reader, cr)
	if err != nil {
		t.Fatalf("readSevenZipMembers: %v", err)
	}
	if !had || creds == 0 || scan.files == 0 {
		t.Fatalf("had=%v creds=%d files=%d, want members parsed", had, creds, scan.files)
	}
	done := prog.DoneBytes()
	if done <= 0 {
		t.Fatalf("DoneBytes = %d; 7z credential members never credited the bar", done)
	}
	if done > weight {
		t.Fatalf("DoneBytes = %d overshot weight %d (scale not applied)", done, weight)
	}
	if done < weight/2 {
		t.Fatalf("DoneBytes = %d of %d (<50%%); crediting too coarse to move the bar", done, weight)
	}
}

func writeEncryptedZipMembers(t *testing.T, path, password string, members map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zipenc.NewWriter(f)
	for name, body := range members {
		w, err := zw.Encrypt(name, password, zipenc.AES256Encryption)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
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

func first7z() string {
	for _, name := range []string{"7z", "7za", "7zr"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func anyContains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
