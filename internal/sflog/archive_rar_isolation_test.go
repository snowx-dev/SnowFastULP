package sflog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func buildInnerZip(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "inner.zip")
	writeZipMembers(t, path, map[string][]byte{
		"inner/Passwords.txt": []byte("URL: https://inner.example/login\nUSER: i\nPASS: p\n"),
	})
	return path
}

// TestRarWrongFirstPasswordNestedFirstMember covers the coupled wrong-password
// + nested-first-member case: the outer RAR's first member is a nested
// archive, the first password candidate is wrong, and the nested spill fails
// mid-body with a wrong-password symptom. That failure must fail the streaming
// attempt (classified wrong password) so readRarCredentials races the
// remaining candidates, instead of recording a parse issue and continuing with
// a truncated nested archive.
func TestRarWrongFirstPasswordNestedFirstMember(t *testing.T) {
	rarBin, _ := exec.LookPath("rar") // empty: fall back to the prebuilt fixture
	for _, withSibling := range []bool{false, true} {
		name := "NestedOnly"
		if withSibling {
			name = "NestedFirstWithSibling"
		}
		t.Run(name, func(t *testing.T) {
			var path string
			if withSibling {
				dir := t.TempDir()
				buildInnerZip(t, dir)
				args := []string{"a", "-prealpw", "-m0", "-idq", "arc.rar", "inner.zip", "Passwords.txt"}
				if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"),
					[]byte("URL: https://outer.example/login\nUSER: o\nPASS: p\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(rarBin, args...)
				cmd.Dir = dir
				if out, e := cmd.CombinedOutput(); e != nil {
					t.Skipf("rar pack failed (%v): %s", e, out)
				}
				path = filepath.Join(dir, "arc.rar")
			} else if rarBin != "" {
				dir := t.TempDir()
				buildInnerZip(t, dir)
				cmd := exec.Command(rarBin, "a", "-prealpw", "-m0", "-idq", "arc.rar", "inner.zip")
				cmd.Dir = dir
				if out, e := cmd.CombinedOutput(); e != nil {
					t.Skipf("rar pack failed (%v): %s", e, out)
				}
				path = filepath.Join(dir, "arc.rar")
			} else {
				// No rar packer: fall back to the committed prebuilt fixture
				// (nested archive only, password "realpw") so the discriminator
				// still runs, per the testdata convention.
				path = filepath.Join("testdata", "nested-only-passwords.rar")
			}

			var creds []Credential
			var issues []Issue
			var dbg []string
			ec := extractCtx{
				passwords: []string{"wrong", "realpw"},
				tempDir:   t.TempDir(),
				display:   path,
				emit:      func(c Credential) { creds = append(creds, c) },
				onIssue: func(p string, k IssueKind, e error) {
					issues = append(issues, Issue{Path: p, Kind: k, Err: e})
				},
				debug: func(f string, a ...any) { dbg = append(dbg, fmt.Sprintf(f, a...)) },
			}

			scan, err := readArchiveCredentials(context.Background(), path, ec, 1<<20)
			if err != nil {
				t.Fatalf("readArchiveCredentials error = %v, want success\ndebug:\n%s", err, joinLines(dbg))
			}
			_ = scan
			urls := credURLs(creds)
			if !anyContains(urls, "https://inner.example/login") {
				t.Fatalf("nested archive creds missing; got %v\ndebug:\n%s", urls, joinLines(dbg))
			}
			if withSibling && !anyContains(urls, "https://outer.example/login") {
				t.Fatalf("outer member creds missing; got %v\ndebug:\n%s", urls, joinLines(dbg))
			}
			for _, is := range issues {
				if is.Kind == IssuePasswordNotFound || is.Kind == IssueParseError {
					t.Fatalf("unexpected issue %+v after the password race recovered\nissues=%+v", is, issues)
				}
			}
			if !anyContains(dbg, "racing 1 candidate(s) on first member") {
				t.Fatalf("password race never ran\ndebug:\n%s", joinLines(dbg))
			}
		})
	}
}

// A pathological credential member (single line beyond the per-line scan cap)
// followed by a valid one must stay isolated: one parse issue for the
// pathological member, the valid member still extracted, no password-not-found.
// Member SIZE alone is no longer a parse failure — large members stream
// (TestRarHugeMemberStreamsAllLines) — so the isolation trigger is the
// pathological line.
func TestRarPathologicalMemberIsolatedBeforeValidMember(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found; skipping oversized-member isolation test")
	}
	dir := t.TempDir()
	// One line longer than the per-line scan cap; -m5 shrinks it on disk but
	// the decoded member still trips bufio.ErrTooLong.
	huge := []byte(strings.Repeat("x", maxScanLineLen+16))
	if err := os.WriteFile(filepath.Join(dir, "PasswordsHuge.txt"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"),
		[]byte("URL: https://valid.example/login\nUSER: v\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// -m5 shrinks the zero blob; the member is oversized uncompressed.
	cmd := exec.Command(rarBin, "a", "-m5", "-idq", "arc.rar", "PasswordsHuge.txt", "Passwords.txt")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	path := filepath.Join(dir, "arc.rar")

	var creds []Credential
	var issues []Issue
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue: func(p string, k IssueKind, e error) {
			issues = append(issues, Issue{Path: p, Kind: k, Err: e})
		},
	}

	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials error = %v, want success", err)
	}
	if len(creds) != 1 || creds[0].URL != "https://valid.example/login" {
		t.Fatalf("credentials = %+v, want the valid member's creds", creds)
	}
	if len(issues) != 1 || issues[0].Kind != IssueParseError ||
		!containsAny(issues[0].Path, "PasswordsHuge.txt") {
		t.Fatalf("issues = %+v, want exactly one parse issue for the oversized member", issues)
	}
	if !errors.Is(issues[0].Err, bufio.ErrTooLong) {
		t.Fatalf("issue err = %v, want the per-line scan-cap overflow", issues[0].Err)
	}
}

// Over-cap nested members in a non-solid RAR skip without decompressing the
// rest of the member (the next Next() reads only packed bytes); the members
// after the nested one still extract. The solid variant must drain to keep the
// decoder aligned and still extract the later member.
func TestRarNestedOverCapLaterMemberSurvives(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found; skipping nested over-cap test")
	}
	for _, solid := range []bool{false, true} {
		name := "NonSolid"
		if solid {
			name = "Solid"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// Incompressible ~1MiB member so the spill trips the tiny member cap
			// long before the nested archive ends.
			big := make([]byte, 1<<20)
			rnd := rand.NewChaCha8([32]byte{42}) // deterministic fixture
			if _, err := rnd.Read(big); err != nil {
				t.Fatal(err)
			}
			innerPath := filepath.Join(dir, "inner.zip")
			writeZipMembers(t, innerPath, map[string][]byte{
				"blob.bin":            big,
				"inner/Passwords.txt": []byte("URL: https://inner.example/login\nUSER: i\nPASS: p\n"),
			})
			if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"),
				[]byte("URL: https://outer.example/login\nUSER: o\nPASS: p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			args := []string{"a", "-m0", "-idq"}
			if solid {
				args = append(args, "-s")
			}
			cmd := exec.Command(rarBin, append(args, "arc.rar", "inner.zip", "Passwords.txt")...)
			cmd.Dir = dir
			if out, e := cmd.CombinedOutput(); e != nil {
				t.Skipf("rar pack failed (%v): %s", e, out)
			}
			path := filepath.Join(dir, "arc.rar")

			var creds []Credential
			var issues []Issue
			ec := extractCtx{
				passwords: []string{""},
				tempDir:   t.TempDir(),
				display:   path,
				emit:      func(c Credential) { creds = append(creds, c) },
				onIssue: func(p string, k IssueKind, e error) {
					issues = append(issues, Issue{Path: p, Kind: k, Err: e})
				},
				// Tiny preset budget so the nested spill over-caps quickly.
				spill: newSpillBudget(64<<10, 64<<10),
			}

			if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
				t.Fatalf("readArchiveCredentials error = %v, want success", err)
			}
			urls := credURLs(creds)
			if !anyContains(urls, "https://outer.example/login") {
				t.Fatalf("later member creds missing after over-cap skip; got %v", urls)
			}
			// The spill aborted at the cap, so the nested archive itself is
			// skipped (issue + continue) in both variants; the solid stream
			// only had to be drained to keep the later member decodable.
			if anyContains(urls, "https://inner.example/login") {
				t.Fatalf("over-capped nested archive should not have been extracted; got %v", urls)
			}
			overCap := 0
			for _, is := range issues {
				if is.Kind == IssueParseError && containsAny(is.Path, "inner.zip") {
					overCap++
				}
			}
			if overCap != 1 {
				t.Fatalf("over-cap issues = %d, want exactly 1\nissues=%+v", overCap, issues)
			}
		})
	}
}

// Same isolation guarantee for 7z: a pathological member (single line beyond
// the per-line scan cap) followed by a valid credential member yields one
// parse issue and the valid member's creds. Member size alone is not a parse
// failure — members stream (see the huge-member tests).
func TestSevenZipPathologicalMemberIsolatedBeforeValidMember(t *testing.T) {
	bin := sevenZipBinary()
	if bin == "" {
		t.Skip("no 7z binary found")
	}
	dir := t.TempDir()
	huge := []byte(strings.Repeat("x", maxScanLineLen+16))
	if err := os.WriteFile(filepath.Join(dir, "PasswordsHuge.txt"), huge, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"),
		[]byte("URL: https://valid.example/login\nUSER: v\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "a", "-bso0", "-bsp0", "arc.7z", "PasswordsHuge.txt", "Passwords.txt")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("7z pack failed (%v): %s", e, out)
	}
	path := filepath.Join(dir, "arc.7z")

	var creds []Credential
	var issues []Issue
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue: func(p string, k IssueKind, e error) {
			issues = append(issues, Issue{Path: p, Kind: k, Err: e})
		},
	}

	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials error = %v, want success", err)
	}
	if len(creds) != 1 || creds[0].URL != "https://valid.example/login" {
		t.Fatalf("credentials = %+v, want the valid member's creds", creds)
	}
	if len(issues) != 1 || issues[0].Kind != IssueParseError ||
		!containsAny(issues[0].Path, "PasswordsHuge.txt") {
		t.Fatalf("issues = %+v, want exactly one parse issue for the oversized member", issues)
	}
	if !errors.Is(issues[0].Err, bufio.ErrTooLong) {
		t.Fatalf("issue err = %v, want the per-line scan-cap overflow", issues[0].Err)
	}
}

// A self-extracting (SFX-prefixed) zip with a valid EOCD must pass the
// signature sniff and extract, while an SFX stub wrapped around non-zip bytes
// stays a rejected decoy.
func TestSFXPrefixedZipAcceptedAndDecoyRejected(t *testing.T) {
	dir := t.TempDir()
	stub := []byte("MZ\x90\x00 self-extracting stub, not a zip header\n")

	// Real zip, prefixed with the stub; central-directory offsets must be
	// adjusted by the stub length.
	inner := filepath.Join(dir, "plain.zip")
	writeZipMembers(t, inner, map[string][]byte{
		"victim/Passwords.txt": []byte("URL: https://sfx.example/login\nUSER: s\nPASS: p\n"),
	})
	raw, err := os.ReadFile(inner)
	if err != nil {
		t.Fatal(err)
	}
	sfx, err := prependSFXStub(raw, len(stub))
	if err != nil {
		t.Fatalf("SFX rewrite failed: %v", err)
	}
	sfxPath := filepath.Join(dir, "installer.zip")
	if err := os.WriteFile(sfxPath, append([]byte{}, append(stub, sfx...)...), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok, err := archiveSignatureOK(sfxPath, ".zip"); err != nil || !ok {
		t.Fatalf("archiveSignatureOK(SFX zip) = (%v, %v), want (true, nil)", ok, err)
	}
	var creds []Credential
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   sfxPath,
		emit:      func(c Credential) { creds = append(creds, c) },
		onIssue:   func(string, IssueKind, error) {},
	}
	if _, err := readArchiveCredentials(context.Background(), sfxPath, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials(SFX zip) error = %v, want success", err)
	}
	if len(creds) != 1 || creds[0].URL != "https://sfx.example/login" {
		t.Fatalf("credentials = %+v, want the SFX member's creds", creds)
	}

	// Decoy: SFX-looking stub around garbage stays rejected.
	decoyPath := filepath.Join(dir, "decoy.zip")
	decoy := append(append([]byte{}, stub...), bytes.Repeat([]byte("junk "), 4000)...)
	if err := os.WriteFile(decoyPath, decoy, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := archiveSignatureOK(decoyPath, ".zip"); err != nil || ok {
		t.Fatalf("archiveSignatureOK(SFX decoy) = (%v, %v), want (false, nil)", ok, err)
	}
}

// prependSFXStub returns the zip with every central-directory local-header
// offset and the EOCD's directory offset shifted by stubLen, so the archive
// still reads with the stub in front.
func prependSFXStub(raw []byte, stubLen int) ([]byte, error) {
	eocd := findEOCDOffset(raw)
	if eocd < 0 {
		return nil, fmt.Errorf("no EOCD found in test zip")
	}
	dirSize := le32(raw, eocd+12)
	dirOff := int(le32(raw, eocd+16))
	if dirOff+int(dirSize) > len(raw) {
		return nil, fmt.Errorf("central directory out of bounds")
	}
	out := append([]byte{}, raw...)
	for p := dirOff; p < dirOff+int(dirSize); {
		if le32(out, p) != 0x02014b50 {
			return nil, fmt.Errorf("unexpected central-directory signature at %d", p)
		}
		nameLen := int(le16(out, p+28))
		extraLen := int(le16(out, p+30))
		commentLen := int(le16(out, p+32))
		oldOff := le32(out, p+42)
		put32(out, p+42, oldOff+uint32(stubLen))
		p += 46 + nameLen + extraLen + commentLen
	}
	put32(out, eocd+16, uint32(dirOff+stubLen))
	return out, nil
}

func findEOCDOffset(b []byte) int {
	const eocdLen = 22
	for i := len(b) - eocdLen; i >= 0; i-- {
		if le32(b, i) == 0x06054b50 {
			commentLen := int(le16(b, i+eocdLen-2))
			if i+eocdLen+commentLen == len(b) {
				return i
			}
		}
	}
	return -1
}

func le16(b []byte, off int) uint16 {
	return uint16(b[off]) | uint16(b[off+1])<<8
}

func le32(b []byte, off int) uint32 {
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

func put32(b []byte, off int, v uint32) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
}

func containsAny(s, sub string) bool {
	return strings.Contains(s, sub)
}

func joinLines(lines []string) string {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}
