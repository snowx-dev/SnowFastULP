package sflog

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concProcessor is a Processor seam shim that sleeps while "processing" a member
// so concurrent member work is observable, recording the peak number of members
// processed at once.
type concProcessor struct {
	delay    time.Duration
	inFlight atomic.Int64
	peak     atomic.Int64
}

func (c *concProcessor) Process(r io.Reader, prov string) ([]Credential, error) {
	n := c.inFlight.Add(1)
	for {
		pk := c.peak.Load()
		if n <= pk || c.peak.CompareAndSwap(pk, n) {
			break
		}
	}
	time.Sleep(c.delay)
	creds, err := ParseCredentials(r, prov)
	c.inFlight.Add(-1)
	return creds, err
}

func writeInnerZip(t *testing.T, path, tag string, blocks int) []string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	w, err := zw.Create("Passwords.txt")
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	var urls []string
	for i := 0; i < blocks; i++ {
		u := fmt.Sprintf("https://%s-%d.example/login", tag, i)
		fmt.Fprintf(&body, "URL: %s\nUSER: u%d\nPASS: p%d\n", u, i, i)
		urls = append(urls, u)
	}
	if _, err := w.Write([]byte(body.String())); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return urls
}

// buildRarOfZips builds a single-file rar holding n inner zips, each with a
// Passwords.txt of `blocks` cred blocks. Returns the rar path and every URL
// packed (for set parity checks).
func buildRarOfZips(t *testing.T, n, blocks int) (string, []string) {
	t.Helper()
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	var names, want []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("inner%02d.zip", i)
		want = append(want, writeInnerZip(t, filepath.Join(dir, name), fmt.Sprintf("z%02d", i), blocks)...)
		names = append(names, name)
	}
	cmd := exec.Command(rarBin, append([]string{"a", "-m0", "-ep1", "-idq", "nested.rar"}, names...)...)
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	return filepath.Join(dir, "nested.rar"), want
}

func collectRarURLs(t *testing.T, rarPath string, sem chan struct{}) []string {
	t.Helper()
	var mu sync.Mutex
	var urls []string
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   rarPath,
		emit:      func(c Credential) { mu.Lock(); urls = append(urls, c.URL); mu.Unlock() },
		onIssue:   func(p string, _ IssueKind, e error) { t.Errorf("unexpected issue %s: %v", p, e) },
		sem:       sem,
	}
	if _, err := readArchiveCredentials(context.Background(), rarPath, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	sort.Strings(urls)
	return urls
}

// TestReadRarNestedMembersRunConcurrently proves the rar producer/consumer split
// actually overlaps work: with a slow Processor and a 4-slot budget, two or more
// nested members are processed at the same time (vs the old one-at-a-time stream).
func TestReadRarNestedMembersRunConcurrently(t *testing.T) {
	rarPath, _ := buildRarOfZips(t, 6, 4)
	p := NewProgress()
	p.SetWorkers(4)
	cp := &concProcessor{delay: 25 * time.Millisecond}
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   rarPath,
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		p:         p,
		sem:       make(chan struct{}, 4),
		processor: cp,
	}
	if _, err := readArchiveCredentials(context.Background(), rarPath, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if cp.peak.Load() < 2 {
		t.Fatalf("nested RAR member peak concurrency = %d, want >= 2 (parallel dispatch not happening)", cp.peak.Load())
	}
}

// TestReadRarNestedParitySequentialVsParallel proves parallel dispatch does not
// change the result: the unique credential set is identical sequential (no
// budget -> all inline) vs parallel (pooled), and matches what was packed.
func TestReadRarNestedParitySequentialVsParallel(t *testing.T) {
	rarPath, want := buildRarOfZips(t, 6, 4)
	sort.Strings(want)
	seq := collectRarURLs(t, rarPath, nil)
	par := collectRarURLs(t, rarPath, make(chan struct{}, 4))
	if !equalStrings(seq, par) {
		t.Fatalf("sequential vs parallel cred sets differ:\nseq=%v\npar=%v", seq, par)
	}
	if !equalStrings(seq, want) {
		t.Fatalf("extracted creds != packed set:\ngot =%v\nwant=%v", seq, want)
	}
}

// TestReadRarEncryptedWordlistResolvesWithoutRestream feeds an encrypted single-
// file rar a wordlist where the first candidate ("") and several decoys are
// wrong. It must resolve via the first-member probe race and run at most two FULL
// passes (the "" attempt + the winner) regardless of wordlist length — the
// regression guard against streaming the whole archive once per candidate.
func TestReadRarEncryptedWordlistResolvesWithoutRestream(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	const pw = "SecretPass42"
	if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"),
		[]byte(credBlocks("enc", 5, 4_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rarBin, "a", "-m0", "-hp"+pw, "-ep1", "-idq", "enc.rar", "Passwords.txt")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar encrypt failed (%v): %s", e, out)
	}
	rarPath := filepath.Join(dir, "enc.rar")

	var dbg []string
	creds := 0
	p := NewProgress()
	p.SetWorkers(4)
	ec := extractCtx{
		passwords: []string{"", "wrong-1", pw, "wrong-2", "wrong-3"},
		tempDir:   t.TempDir(),
		display:   rarPath,
		emit:      func(Credential) { creds++ },
		onIssue:   func(pth string, _ IssueKind, e error) { t.Errorf("unexpected issue %s: %v", pth, e) },
		p:         p,
		sem:       make(chan struct{}, 4),
		debug:     func(f string, a ...any) { dbg = append(dbg, fmt.Sprintf(f, a...)) },
	}
	if _, err := readArchiveCredentials(context.Background(), rarPath, ec, 4_000); err != nil {
		t.Fatalf("encrypted rar with good pw in wordlist failed: %v\ndebug:\n%s", err, strings.Join(dbg, "\n"))
	}
	if creds == 0 {
		t.Fatal("no creds emitted from a resolvable encrypted rar")
	}
	// ": extracting" matches the two pass lines ("extracting" and "extracting
	// with resolved password") but not the "still extracting" heartbeat.
	if full := countContains(dbg, ": extracting"); full > 2 {
		t.Fatalf("full extraction passes = %d, want <= 2 (whole-archive re-stream regression)\ndebug:\n%s",
			full, strings.Join(dbg, "\n"))
	}
	if anyContains(dbg, "testing password") {
		t.Fatalf("stale 'testing password' wording present\ndebug:\n%s", strings.Join(dbg, "\n"))
	}
	if !anyContains(dbg, "racing") {
		t.Fatalf("expected the parallel probe race to engage\ndebug:\n%s", strings.Join(dbg, "\n"))
	}
}

// TestReadRarVolumesNestedMembersRunConcurrently is the multi-volume counterpart
// to TestReadRarNestedMembersRunConcurrently: it proves the OpenReader (volume)
// path also overlaps nested-member processing, so a big multi-part set's tail
// uses 2+ cores instead of one.
func TestReadRarVolumesNestedMembersRunConcurrently(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	var names []string
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("inner%02d.zip", i)
		writeInnerZip(t, filepath.Join(dir, name), fmt.Sprintf("v%02d", i), 300)
		names = append(names, name)
	}
	cmd := exec.Command(rarBin, append([]string{"a", "-m0", "-v32k", "-ep1", "-idq", "vol.rar"}, names...)...)
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "vol.part*.rar"))
	sort.Strings(parts)
	if len(parts) < 2 {
		t.Skipf("rar produced %d part(s), need a real multi-volume set", len(parts))
	}
	var weight int64
	for _, pp := range parts {
		fi, e := os.Stat(pp)
		if e != nil {
			t.Fatal(e)
		}
		weight += fi.Size()
	}

	p := NewProgress()
	p.SetWorkers(4)
	cp := &concProcessor{delay: 25 * time.Millisecond}
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   parts[0],
		emit:      func(Credential) {},
		onIssue:   func(string, IssueKind, error) {},
		p:         p,
		sem:       make(chan struct{}, 4),
		processor: cp,
	}
	if _, err := readRarVolumes(context.Background(), parts, ec, weight); err != nil {
		t.Fatalf("readRarVolumes: %v", err)
	}
	if cp.peak.Load() < 2 {
		t.Fatalf("multi-volume nested member peak concurrency = %d, want >= 2", cp.peak.Load())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
