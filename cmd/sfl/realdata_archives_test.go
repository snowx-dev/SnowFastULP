package main

// Real-data archive + concurrency suite, gated on SFL_REALDATA_DIR like the
// rest of the RealData tests. It drives the cherry-picked fixtures built by
// scripts/build-sfl-fixtures.sh (one archive per type, plus password lists) and
// asserts: every archive type agrees with the plain baseline, bad passwords are
// handled gracefully and surface in the end summary, and the engine's
// parallelism is actually observable through the live worker registry.
//
// Run: SFL_REALDATA_DIR=/path/to/ulp go test ./cmd/sfl -run RealData -v

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// cherryExpected reads the unique-credential baseline the fixture script wrote
// from the plain zip, skipping if the fixtures haven't been built.
func cherryExpected(t *testing.T, root string) int {
	t.Helper()
	p := filepath.Join(fixturesDir(t, root), "cherry-expected.txt")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("missing %s (run scripts/build-sfl-fixtures.sh)", p)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		t.Fatalf("bad cherry-expected.txt %q: %v", string(b), err)
	}
	return n
}

// cherryArchiveTypes is every cherry fixture; .rar is present only when a packer
// was available at build time, so callers requireFile (skip) each one.
var cherryEncryptedTypes = []string{"cherry-encrypted.zip", "cherry-encrypted.7z", "cherry-encrypted.rar"}

// TestRealDataCherryArchivesPerType proves each archive type extracts the same
// credential set as the plain baseline with the good password (loadPw(goodPass)
// yields ["", goodPass], so it also opens the unencrypted plain zip).
func TestRealDataCherryArchivesPerType(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	want := cherryExpected(t, root)
	pw := loadPw(t, goodPass)

	for _, name := range append([]string{"cherry-plain.zip"}, cherryEncryptedTypes...) {
		t.Run(name, func(t *testing.T) {
			arc := requireFile(t, filepath.Join(fx, name))
			res := runEngine(t, arc, pw, false)
			if res.stats.PasswordNotFound != 0 {
				t.Fatalf("%s: PasswordNotFound = %d, want 0", name, res.stats.PasswordNotFound)
			}
			if res.stats.Emitted != want {
				t.Fatalf("%s: Emitted = %d, want %d (every type must agree with the plain baseline)",
					name, res.stats.Emitted, want)
			}
			assertValidULP(t, res.ulp)
		})
	}
}

// TestRealDataCherryPasswordsManyFile uses a wordlist with the good password
// buried among non-working ones: extraction must still succeed, proving bad
// candidates are tried and skipped gracefully regardless of order.
func TestRealDataCherryPasswordsManyFile(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	want := cherryExpected(t, root)
	pwFile := requireFile(t, filepath.Join(fx, "passwords-many.txt"))

	for _, name := range cherryEncryptedTypes {
		t.Run(name, func(t *testing.T) {
			arc := requireFile(t, filepath.Join(fx, name))
			res := runEngine(t, arc, loadPw(t, pwFile), false)
			if res.stats.Emitted != want || res.stats.PasswordNotFound != 0 {
				t.Fatalf("%s many-pw: Emitted=%d PasswordNotFound=%d, want %d/0",
					name, res.stats.Emitted, res.stats.PasswordNotFound, want)
			}
		})
	}
}

// TestRealDataCherryBadOnlyPasswordKept feeds only non-working passwords: each
// encrypted archive yields nothing, is reported as password-not-found, stays
// not-OK (so -del never discards un-extracted data), and records an issue.
func TestRealDataCherryBadOnlyPasswordKept(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	pwFile := requireFile(t, filepath.Join(fx, "passwords-bad-only.txt"))

	for _, name := range cherryEncryptedTypes {
		t.Run(name, func(t *testing.T) {
			arc := requireFile(t, filepath.Join(fx, name))
			res := runEngine(t, arc, loadPw(t, pwFile), false)
			if res.stats.Emitted != 0 {
				t.Fatalf("%s bad-only: Emitted=%d, want 0", name, res.stats.Emitted)
			}
			if res.stats.PasswordNotFound != 1 || res.stats.SkippedArchives != 1 {
				t.Fatalf("%s bad-only: PasswordNotFound=%d SkippedArchives=%d, want 1/1",
					name, res.stats.PasswordNotFound, res.stats.SkippedArchives)
			}
			if len(res.results) != 1 || res.results[0].OK {
				t.Fatalf("%s bad-only: results=%+v, want one not-OK archive (kept, not -del eligible)",
					name, res.results)
			}
			var sawPNF bool
			for _, is := range res.stats.Issues {
				if is.Kind == sflog.IssuePasswordNotFound {
					sawPNF = true
				}
			}
			if !sawPNF {
				t.Fatalf("%s bad-only: expected password-not-found issue, got %+v", name, res.stats.Issues)
			}
		})
	}
}

// TestRealDataCherrySummaryShowsFails runs a folder holding an openable plain
// zip and a locked encrypted zip with bad-only passwords, then asserts the
// rendered end summary surfaces the failure (count row + per-kind line).
func TestRealDataCherrySummaryShowsFails(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	input := t.TempDir()
	copyFile(t, requireFile(t, filepath.Join(fx, "cherry-plain.zip")), filepath.Join(input, "cherry-plain.zip"))
	copyFile(t, requireFile(t, filepath.Join(fx, "cherry-encrypted.zip")), filepath.Join(input, "cherry-encrypted.zip"))

	res := runEngine(t, input, loadPw(t, requireFile(t, filepath.Join(fx, "passwords-bad-only.txt"))), false)
	if res.stats.PasswordNotFound < 1 || res.stats.SkippedArchives < 1 {
		t.Fatalf("expected a locked archive: PasswordNotFound=%d SkippedArchives=%d",
			res.stats.PasswordNotFound, res.stats.SkippedArchives)
	}
	joined := strings.Join(renderFinalSummary("out/sfl.txt", res.stats), "\n")
	for _, want := range []string{"password not found", "skipped"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("end summary missing %q:\n%s", want, joined)
		}
	}
}

// TestRealDataSurfacesConcurrency proves the engine really processes multiple
// sources at once AND that it is observable through the worker registry the TUI
// reads: a background sampler watches Progress.ActiveWorkers while a batch of
// victim folders is extracted and asserts peak concurrency >= 2.
func TestRealDataSurfacesConcurrency(t *testing.T) {
	root := realDataRoot(t)
	workers := runtime.GOMAXPROCS(0)
	if workers < 2 {
		t.Skip("need >=2 cores to observe concurrent workers")
	}
	input := t.TempDir()
	if n := copyNVictims(t, victimsParent(t, root), input, 16); n < 2 {
		t.Skip("need >=2 victim folders to observe concurrency")
	}

	prog := sflog.NewProgress()
	var peak atomic.Int64
	done := make(chan struct{})
	var sampWG sync.WaitGroup
	sampWG.Add(1)
	go func() {
		defer sampWG.Done()
		for {
			select {
			case <-done:
				return
			default:
				if got := int64(len(prog.ActiveWorkers(workers))); got > peak.Load() {
					peak.Store(got)
				}
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()

	eng := &sflog.Engine{Workers: workers, Progress: prog, Passwords: []string{""}}
	var out strings.Builder
	_, _, err := eng.Run(context.Background(), input, &out)
	close(done)
	sampWG.Wait()
	if err != nil {
		t.Fatalf("engine run: %v", err)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrent workers = %d, want >= 2 (parallelism not surfaced)", peak.Load())
	}
	t.Logf("peak concurrent workers observed: %d (of %d)", peak.Load(), workers)
}
