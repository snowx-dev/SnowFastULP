package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// H-22 regression tests: a multipart commit is one transaction. A failure on
// a later part must not leave a mixed archive set: destinations are
// preflighted, existing outputs are backed up before the first publish, and
// any post-publish failure restores the exact pre-run state.

// TestChunkedCommitRefusesNonRegularDestinationUpFront: a directory parked at
// a later part's final path can never be atomically replaced, so the whole
// batch must fail before ANY part is published and before ANY pre-existing
// output is touched.
func TestChunkedCommitRefusesNonRegularDestinationUpFront(t *testing.T) {
	dir := t.TempDir()
	stamp := "20260928_preflight"

	// plant a good old part 1 — the review's "old part 1" that must survive
	oldPart1 := zstPartPath(dir, stamp, 1)
	writeAtomicSentinel(t, oldPart1)

	c, err := newChunkedZstdSink(dir, stamp, 1, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLine(c, "https://a.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	if err := writeLine(c, "https://b.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	// sabotage part 2's publish target
	part2 := zstPartPath(dir, stamp, 2)
	if err := os.MkdirAll(part2, 0o755); err != nil {
		t.Fatal(err)
	}

	err = c.commit()
	if err == nil {
		t.Fatal("commit must fail when a destination is not a regular file")
	}
	if !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), filepath.Base(part2)) {
		t.Fatalf("error must name the offending destination: %v", err)
	}

	// nothing was published: the old part 1 is byte-identical and no new
	// part 1 file replaced it
	if got := readAtomicFile(t, oldPart1); got != atomicSentinel {
		t.Fatalf("old part 1 was modified by the refused batch: %q", got)
	}
	if fi, serr := os.Stat(part2); serr != nil || !fi.IsDir() {
		t.Fatalf("pre-existing part-2 directory must be untouched: %v", serr)
	}
	assertNoTempLeftovers(t, dir)
}

// TestChunkedCommitRollbackRestoresPreviousParts drives the rollback leg
// directly: after part 1 has been published over an old output, a part-2
// failure must restore the old part 1 byte-for-byte and remove destinations
// that did not exist before.
func TestChunkedCommitRollbackRestoresPreviousParts(t *testing.T) {
	dir := t.TempDir()
	stamp := "20260928_rollback"

	oldPart1 := zstPartPath(dir, stamp, 1)
	writeAtomicSentinel(t, oldPart1)

	c, err := newChunkedZstdSink(dir, stamp, 1, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLine(c, "https://a.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	if err := writeLine(c, "https://b.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.seal(); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// simulate the transaction's failure point: back up destinations, publish
	// part 1 over the old output, then roll back as a failing part 2 would
	backups, err := c.backupDestinations()
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := c.sealed[0].commit(); err != nil {
		t.Fatalf("part 1 publish: %v", err)
	}
	if got := readAtomicFile(t, oldPart1); got == atomicSentinel {
		t.Fatal("test setup: part 1 was not actually replaced")
	}
	// a failing part 2 aborts the unpublished parts before the restore
	if err := c.sealed[1].abort(); err != nil {
		t.Fatalf("abort part 2: %v", err)
	}
	if err := c.restoreDestinations(backups); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := readAtomicFile(t, oldPart1); got != atomicSentinel {
		t.Fatalf("old part 1 not restored after rollback: %q", got)
	}
	part2 := zstPartPath(dir, stamp, 2)
	if _, err := os.Stat(part2); !os.IsNotExist(err) {
		t.Fatalf("never-published part 2 must not exist after rollback: %v", err)
	}
	assertNoTempLeftovers(t, dir)
}
