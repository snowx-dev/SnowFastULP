package sflog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestGatedSinkBuffersUntilConfirm covers the core contract: nothing reaches the
// real sinks before confirm, confirm flushes what was held in order and then
// passes through, and a second confirm is a no-op.
func TestGatedSinkBuffersUntilConfirm(t *testing.T) {
	var emitted []string
	var issued []IssueKind
	gated, g := newGatedSink(extractCtx{
		emit:    func(c Credential) { emitted = append(emitted, c.URL) },
		onIssue: func(_ string, k IssueKind, _ error) { issued = append(issued, k) },
	})

	gated.emit(Credential{URL: "a"})
	gated.onIssue("x", IssueParseError, errors.New("boom"))
	gated.emit(Credential{URL: "b"})
	if len(emitted) != 0 || len(issued) != 0 {
		t.Fatalf("pre-confirm leaked to real sinks: emitted=%v issued=%v", emitted, issued)
	}

	gated.confirmPassword()
	if want := []string{"a", "b"}; !equalStrings(emitted, want) {
		t.Fatalf("flush order = %v, want %v", emitted, want)
	}
	if len(issued) != 1 || issued[0] != IssueParseError {
		t.Fatalf("issues after flush = %v, want [IssueParseError]", issued)
	}

	gated.emit(Credential{URL: "c"})
	if want := []string{"a", "b", "c"}; !equalStrings(emitted, want) {
		t.Fatalf("passthrough after confirm = %v, want %v", emitted, want)
	}

	g.confirm() // idempotent: must not re-flush
	if want := []string{"a", "b", "c"}; !equalStrings(emitted, want) {
		t.Fatalf("second confirm re-flushed: %v", emitted)
	}
}

// TestGatedSinkUnconfirmedDropsBuffer proves a wrong password (never confirmed)
// discards its partial output instead of leaking garbage.
func TestGatedSinkUnconfirmedDropsBuffer(t *testing.T) {
	var emitted []string
	gated, _ := newGatedSink(extractCtx{
		emit:    func(c Credential) { emitted = append(emitted, c.URL) },
		onIssue: func(string, IssueKind, error) {},
	})
	gated.emit(Credential{URL: "wrong-pw-garbage"})
	if len(emitted) != 0 {
		t.Fatalf("unconfirmed gate emitted %v, want nothing", emitted)
	}
}

// countingProcessor counts Process calls (one per credential member) so an emit
// callback can observe how many members were parsed when the first cred arrives.
type countingProcessor struct{ parsed atomic.Int64 }

func (c *countingProcessor) Process(r io.Reader, prov string) ([]Credential, error) {
	c.parsed.Add(1)
	return ParseCredentials(r, prov)
}

// firstEmitObserver records how many members had been parsed at the first emit.
// Buffer-and-commit shows all members parsed (== total, i.e. at EOF); gated
// streaming shows only the first member proven (< total).
func firstEmitObserver(cp *countingProcessor, total *int) (func(Credential), *int64) {
	first := new(int64)
	atomic.StoreInt64(first, -1)
	return func(Credential) {
		if atomic.LoadInt64(first) < 0 {
			atomic.StoreInt64(first, cp.parsed.Load())
		}
		*total++
	}, first
}

// TestReadRarSingleEmitsIncrementally proves a single-file rar streams creds as
// each member decodes rather than dumping them all at EOF.
func TestReadRarSingleEmitsIncrementally(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	const members = 6
	var names []string
	for i := 0; i < members; i++ {
		n := fmt.Sprintf("Passwords-%02d.txt", i)
		names = append(names, n)
		if err := os.WriteFile(filepath.Join(dir, n),
			[]byte(credBlocks(fmt.Sprintf("s%02d", i), 3, 2_000)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(rarBin, append([]string{"a", "-m0", "-ep1", "-idq", "single.rar"}, names...)...)
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	rarPath := filepath.Join(dir, "single.rar")

	cp := &countingProcessor{}
	total := 0
	emit, first := firstEmitObserver(cp, &total)
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   rarPath,
		emit:      emit,
		onIssue:   func(p string, _ IssueKind, e error) { t.Errorf("unexpected issue %s: %v", p, e) },
		processor: cp,
	}
	if _, err := readArchiveCredentials(context.Background(), rarPath, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if total == 0 {
		t.Fatal("no creds emitted")
	}
	if got := atomic.LoadInt64(first); got < 1 || got >= int64(members) {
		t.Fatalf("first cred emitted after %d/%d members parsed; want incremental (>=1, <%d)",
			got, members, members)
	}
}

// TestReadSevenZipEmitsIncrementally is the 7z counterpart: creds stream after
// the first member confirms the password rather than all at once on clean EOF.
func TestReadSevenZipEmitsIncrementally(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	const members = 6
	var names []string
	for i := 0; i < members; i++ {
		n := fmt.Sprintf("Passwords-%02d.txt", i)
		names = append(names, n)
		if err := os.WriteFile(filepath.Join(dir, n),
			[]byte(credBlocks(fmt.Sprintf("z%02d", i), 3, 2_000)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bin, append([]string{"a", "-y", "-bso0", "-bsp0", "out.7z"}, names...)...)
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("7z create failed: %v\n%s", e, out)
	}
	path := filepath.Join(dir, "out.7z")

	cp := &countingProcessor{}
	total := 0
	emit, first := firstEmitObserver(cp, &total)
	ec := extractCtx{
		passwords: []string{""},
		tempDir:   t.TempDir(),
		display:   path,
		emit:      emit,
		onIssue:   func(p string, _ IssueKind, e error) { t.Errorf("unexpected issue %s: %v", p, e) },
		processor: cp,
	}
	if _, err := readArchiveCredentials(context.Background(), path, ec, 1<<20); err != nil {
		t.Fatalf("readArchiveCredentials: %v", err)
	}
	if total == 0 {
		t.Fatal("no creds emitted")
	}
	if got := atomic.LoadInt64(first); got < 1 || got >= int64(members) {
		t.Fatalf("first cred emitted after %d/%d members parsed; want incremental (>=1, <%d)",
			got, members, members)
	}
}
