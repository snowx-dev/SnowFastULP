package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The failed -del report must split the intended roots into what was already
// irreversibly removed and what remains, comparing canonical paths so relative
// and absolute spellings of the same file match.
func TestDeletePartialSummarySplitsAndCanonicalizes(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	c := filepath.Join(dir, "c.txt")

	outcome := summarizeDeletion(
		[]string{a, b, c},
		[]string{filepath.Join(dir, ".", "a.txt"), a}, // dirty spelling + exact dup
	)
	if !reflect.DeepEqual(outcome.Deleted, []string{a}) {
		t.Fatalf("deleted = %v, want canonical [%s]", outcome.Deleted, a)
	}
	if !reflect.DeepEqual(outcome.Remaining, []string{b, c}) {
		t.Fatalf("remaining = %v, want [%s %s]", outcome.Remaining, b, c)
	}
	if len(outcome.Intended) != 3 {
		t.Fatalf("intended = %v, want 3 canonical roots", outcome.Intended)
	}
}

// The stderr block must be deterministic: same inputs, same lines, with the
// counts and every path spelled out under the fixed headings.
func TestDeletePartialRenderBlockIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	outcome := summarizeDeletion([]string{a, b}, []string{b})
	cause := errors.New("remove failed")

	lines := renderDeletionOutcome(outcome, cause)
	again := renderDeletionOutcome(outcome, cause)
	if !reflect.DeepEqual(lines, again) {
		t.Fatalf("render is not deterministic:\n%v\n%v", lines, again)
	}
	if lines[0] != "sfu: delete inputs: remove failed" {
		t.Fatalf("first line = %q", lines[0])
	}
	joined := ""
	for _, ln := range lines {
		joined += ln + "\n"
	}
	for _, want := range []string{
		"  deleted before failure (1):",
		"    " + b,
		"  not deleted (1):",
		"    " + a,
	} {
		if !contains(lines, want) {
			t.Fatalf("block missing %q:\n%s", want, joined)
		}
	}
}

// The JSON error terminal carries the same counts as the stderr block.
func TestDeletePartialJSONMessageCarriesCounts(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	c := filepath.Join(dir, "c.txt")
	outcome := summarizeDeletion([]string{a, b, c}, []string{a})
	msg := deletionJSONMessage(outcome, errors.New("remove failed"))
	for _, want := range []string{"remove failed", "deleted 1 of 3 source(s) before failure; 2 not deleted"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("json message %q missing %q", msg, want)
		}
	}
}

// The intended-root computation must skip the run's own outputs by canonical
// path and by filesystem identity (hardlink/symlink aliases), exactly like the
// deletion it mirrors.
func TestDeletePartialIntendedRootsSkipOutputs(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	alias := filepath.Join(dir, "alias.txt")
	out := filepath.Join(dir, "sfu_out.txt.zst")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, alias); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	got, err := intendedDeletionRoots([]string{a, alias, out}, []string{out})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{a, alias}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roots = %v, want %v", got, want)
	}

	// An output that is a filesystem identity alias of an input protects it.
	got, err = intendedDeletionRoots([]string{a, alias}, []string{alias})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("roots = %v, want none (both are the output's identity)", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
