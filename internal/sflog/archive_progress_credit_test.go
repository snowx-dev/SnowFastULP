package sflog

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	zipenc "github.com/yeka/zip"
)

// buildTdataZip makes a zip with one tiny cred member and one big tdata-prefixed
// member (key_datas sibling present so the tree is "confirmed").
func buildTdataZip(t *testing.T, dir string, tdataSize int) string {
	t.Helper()
	path := filepath.Join(dir, "tg.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zipenc.NewWriter(f)
	cw, err := zw.Create("Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cw.Write([]byte("http://a.com:u:p\n")); err != nil {
		t.Fatal(err)
	}
	tw, err := zw.Create("tdata/key_datas")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("k")); err != nil {
		t.Fatal(err)
	}
	bw, err := zw.Create("tdata/D877F783D5D3EF8C/C520CFA523E51015s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bw.Write(bytes.Repeat([]byte("x"), tdataSize)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Regression: the progress denominator must include tdata/env member bytes,
// else the creditor's scale (full weight / cred-bytes-only) lets the tiny
// credential pass saturate the bar while the tdata copy tail is still silent.
func TestZipWeightDenominatorIncludesTdata(t *testing.T) {
	dir := t.TempDir()
	const tdataSize = 4 << 20 // 4 MiB, dwarfs the cred member
	path := buildTdataZip(t, dir, tdataSize)
	weight := fileWeight(path)

	p := NewProgress()
	p.SetWorkers(1)
	// Sample DoneBytes when the credential member is emitted: the cred pass
	// has just finished and the tdata copy has not started. The cred member is
	// ~17B of the ~4MiB+ weight, so old code (denominator = cred bytes only,
	// scale ~weight/17) already credits ~100% of weight here; new code must
	// leave most of the budget for the tdata member's bytes.
	var atEmit int64
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   dir,
		display:   path,
		p:         p,
		env:       NewEnvCopier(filepath.Join(dir, "env-dest"), p, EnvCopyMaxLen),
		emit:      func(Credential) { atEmit = p.DoneBytes() },
		onIssue:   func(string, IssueKind, error) {},
	}
	scan, err := readArchiveCredentials(context.Background(), path, ec, weight)
	if err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if scan.files != 1 {
		t.Fatalf("scan.files = %d, want 1 (the cred member)", scan.files)
	}
	if atEmit > weight/2 {
		t.Fatalf("DoneBytes = %d after the cred pass, want <= half of weight %d (denominator ignores tdata members)", atEmit, weight)
	}
	// After the run the creditor's finish() tops the remainder, so the full
	// weight must be credited: cred + tdata together.
	if got := p.DoneBytes(); got != weight {
		t.Fatalf("final DoneBytes = %d, want weight %d", got, weight)
	}
}

// TestZipCopyTailLabelShowsCopying proves the worker row switches to a
// "copying N file(s)" label during the tdata/env copy tail instead of staying
// on the stale members-left count.
func TestZipCopyTailLabelShowsCopying(t *testing.T) {
	dir := t.TempDir()
	path := buildTdataZip(t, dir, 1<<20)

	p := NewProgress()
	p.SetWorkers(1)
	var mu sync.Mutex
	var items []string
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   dir,
		display:   path,
		p:         p,
		env:       NewEnvCopier(filepath.Join(dir, "env-dest"), p, EnvCopyMaxLen),
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		setItem:   func(s string) { mu.Lock(); defer mu.Unlock(); items = append(items, s) },
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec, fileWeight(path)); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	for _, it := range items {
		if strings.Contains(it, " copying ") && strings.Contains(it, "file(s)") {
			return
		}
	}
	t.Fatalf("no copying label published during the copy tail; items=%q", items)
}

// buildTdata7z packs a plain 7z with one tiny cred member and one big
// tdata-prefixed member (key_datas sibling present so the tree is confirmed).
// Requires the 7z packer; skips otherwise (same gate as the other 7z tests).
func buildTdata7z(t *testing.T, dir string, tdataSize int) string {
	t.Helper()
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"), []byte("http://a.com:u:p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	td := filepath.Join(dir, "tdata", "D877F783D5D3EF8C")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tdata", "key_datas"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "C520CFA523E51015s"), bytes.Repeat([]byte("x"), tdataSize), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "a", "-t7z", "-mx=0", "-bd", "plain.7z", "Passwords.txt", "tdata")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("7z create failed: %v\n%s", err, out)
	}
	return filepath.Join(dir, "plain.7z")
}

// Same regression as TestZipWeightDenominatorIncludesTdata on the 7z path: the
// cred pass must not saturate the bar while the tdata copy tail is pending.
func TestSevenZipWeightDenominatorIncludesTdata(t *testing.T) {
	dir := t.TempDir()
	const tdataSize = 4 << 20 // 4 MiB, dwarfs the cred member
	path := buildTdata7z(t, dir, tdataSize)
	weight := fileWeight(path)

	p := NewProgress()
	p.SetWorkers(1)
	var atEmit int64
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   dir,
		display:   path,
		p:         p,
		env:       NewEnvCopier(filepath.Join(dir, "env-dest"), p, EnvCopyMaxLen),
		emit:      func(Credential) { atEmit = p.DoneBytes() },
		onIssue:   func(string, IssueKind, error) {},
	}
	scan, err := readArchiveCredentials(context.Background(), path, ec, weight)
	if err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if scan.files != 1 {
		t.Fatalf("scan.files = %d, want 1 (the cred member)", scan.files)
	}
	if atEmit > weight/2 {
		t.Fatalf("DoneBytes = %d after the cred pass, want <= half of weight %d (denominator ignores tdata members)", atEmit, weight)
	}
	if got := p.DoneBytes(); got != weight {
		t.Fatalf("final DoneBytes = %d, want weight %d", got, weight)
	}
}
