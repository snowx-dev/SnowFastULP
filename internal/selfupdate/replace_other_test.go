//go:build !windows

package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// replaceExistingBinTestTarget writes an initial old-style binary at dir/name
// and returns the target path.
func replaceExistingBinTestTarget(t *testing.T, dir, name string) string {
	t.Helper()
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte("old-binary-payload\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return target
}

// TestReplaceExistingBinTargetNeverMissing pins the H-05 contract: the target
// pathname must be present at every observable point across many successive
// replaces. The previous two-rename implementation (target → .target.old,
// then .target.new → target) leaves the pathname absent for a window between
// the renames, so a concurrent observer (a process manager, a launcher, a
// crash) can catch the executable missing.
func TestReplaceExistingBinTargetNeverMissing(t *testing.T) {
	dir := t.TempDir()
	target := replaceExistingBinTestTarget(t, dir, "sfu")

	stop := make(chan struct{})
	misses := make(chan string, 64)
	pollerDone := make(chan struct{})
	go func() {
		defer close(pollerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(target); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					select {
					case misses <- "target missing":
					default:
					}
				} else {
					select {
					case misses <- "stat failed: " + err.Error():
					default:
					}
				}
			}
			runtime.Gosched()
		}
	}()

	const iterations = 300
	for i := 0; i < iterations; i++ {
		data := []byte(strings.Repeat("payload-", 8) + "\n")
		want := sha256.Sum256(data)
		if err := replaceExistingBin(data, target, want[:]); err != nil {
			close(stop)
			<-pollerDone
			t.Fatalf("replace %d failed: %v", i, err)
		}
	}

	close(stop)
	<-pollerDone
	select {
	case m := <-misses:
		t.Fatalf("target pathname went missing during %d replaces: %s", iterations, m)
	default:
	}
}

// TestReplaceExistingBinPreservesMode pins that the replaced target keeps the
// pre-replace permission mode (a distro-installed binary may be 0751, not the
// 0755 default of a freshly written temp file).
func TestReplaceExistingBinPreservesMode(t *testing.T) {
	dir := t.TempDir()
	target := replaceExistingBinTestTarget(t, dir, "sfu")
	if err := os.Chmod(target, 0o751); err != nil {
		t.Fatal(err)
	}

	data := []byte("new-binary-payload\n")
	want := sha256.Sum256(data)
	if err := replaceExistingBin(data, target, want[:]); err != nil {
		t.Fatalf("replace failed: %v", err)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o751 {
		t.Fatalf("target mode changed: got %o, want 751", got)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("target content wrong: got %q", got)
	}
}

// TestReplaceExistingBinNoLeftovers pins that a successful replace leaves the
// directory clean: no ".target.old"-style backups, no hidden temp payloads.
func TestReplaceExistingBinNoLeftovers(t *testing.T) {
	dir := t.TempDir()
	target := replaceExistingBinTestTarget(t, dir, "sfu")

	data := []byte("new-binary-payload\n")
	want := sha256.Sum256(data)
	if err := replaceExistingBin(data, target, want[:]); err != nil {
		t.Fatalf("replace failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var leftovers []string
	for _, e := range entries {
		name := e.Name()
		if name == "sfu" {
			continue
		}
		if strings.HasSuffix(name, ".old") || strings.HasPrefix(name, ".sfu.") ||
			strings.HasPrefix(name, ".replace-") || strings.HasPrefix(name, ".newbin-") {
			leftovers = append(leftovers, name)
		}
	}
	if len(leftovers) > 0 {
		t.Fatalf("leftover files after replace: %v", leftovers)
	}
}

// TestReplaceExistingBinFailingWriteLeavesOldIntact pins that a replace which
// cannot write (read-only install dir) fails without touching the old binary.
func TestReplaceExistingBinFailingWriteLeavesOldIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	target := replaceExistingBinTestTarget(t, dir, "sfu")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	oldBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	data := []byte("new-binary-payload\n")
	want := sha256.Sum256(data)
	if err := replaceExistingBin(data, target, want[:]); err == nil {
		t.Fatal("replace in read-only dir unexpectedly succeeded")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("old target unreadable after failed replace: %v", err)
	}
	if !bytes.Equal(got, oldBytes) {
		t.Fatal("failed replace modified the old target")
	}
}
