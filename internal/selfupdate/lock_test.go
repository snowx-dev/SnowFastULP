package selfupdate

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAcquireUpdateLockExclusive proves a second acquire on a live lock fails
// cleanly with a pid-bearing message and does not remove or rewrite the
// holder's lock file.
func TestAcquireUpdateLockExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	lockPath := filepath.Join(dir, lockFileName)
	before, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if !strings.Contains(string(before), "pid=") || !strings.Contains(string(before), "started=") {
		t.Fatalf("lock content = %q, want pid= and started= fields", before)
	}

	_, err = acquireUpdateLock(dir)
	if err == nil {
		t.Fatal("second acquire succeeded, want held-lock error")
	}
	if !strings.Contains(err.Error(), "another update is in progress") {
		t.Fatalf("err = %v, want in-progress message", err)
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Fatalf("err = %v, want holder pid", err)
	}
	if _, isHeld := lockHeldError(err); !isHeld {
		t.Fatalf("lockHeldError missing for %v", err)
	}

	// The loser must not have clobbered the winner's lock.
	after, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock after failed acquire: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("lock content changed: %q -> %q", before, after)
	}

	release()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock still present after release, err=%v", err)
	}
}

// TestUpdateLockStealWhenStale proves a lock older than the staleness window
// is stolen by a fresh updater instead of blocking it forever.
func TestUpdateLockStealWhenStale(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)
	stale := "pid=999999\nstarted=" + time.Now().Add(-2*lockStaleAge).UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(lockPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("stale lock not stolen: %v", err)
	}
	defer release()

	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "pid=") || strings.Contains(string(got), "999999") {
		t.Fatalf("lock = %q, want a rewritten file with the new pid", got)
	}
}

// TestUpdateLockStealByMtime covers a lock whose content carries no parseable
// timestamp (crashed writer, external junk): staleness falls back to mtime.
func TestUpdateLockStealByMtime(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)
	if err := os.WriteFile(lockPath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStaleAge)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	release, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("mtime-stale lock not stolen: %v", err)
	}
	defer release()
}

// TestUpdateLockRemovedOnFailurePath proves the deferred release runs on the
// error path: after a run that fails during apply, the lock file is gone and
// the next run can proceed.
func TestUpdateLockRemovedOnFailurePath(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, []byte("new-sfu"), []byte("new-sfs"))
	defer srv.Close()

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	applyPayloadHook = func(_ []byte, _ string, _ []byte) error {
		return fmt.Errorf("disk full")
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	runErr := run(nil, "0.1.1", "sfu", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	})
	if runErr == nil {
		t.Fatal("expected apply failure")
	}
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); !os.IsNotExist(err) {
		t.Fatalf("lock not removed after failed run, err=%v", err)
	}

	// The next run acquires cleanly (hook cleared by the deferred cleanup
	// order — run the success probe with a fresh apply hook that succeeds).
	applyPayloadHook = nil
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("follow-up run blocked by leftover lock: %v", err)
	}
	assertFileBytes(t, filepath.Join(dir, "sfu"+exeExt()), []byte("new-sfu"))
}

// TestRunConcurrentUpdatersFailCleanly simulates two updaters racing: the
// first holds the lock through its apply phase, the second must abort with
// the in-progress message instead of also swapping binaries.
func TestRunConcurrentUpdatersFailCleanly(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, []byte("new-sfu"), []byte("new-sfs"))
	defer srv.Close()

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")

	entered := make(chan struct{})
	leave := make(chan struct{})
	applied := make(chan string, 4)
	applyPayloadHook = func(_ []byte, target string, _ []byte) error {
		select {
		case <-entered:
		default:
			close(entered)
			<-leave // hold the apply phase until the rival has tried
		}
		applied <- filepath.Base(target)
		return nil
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	var wg sync.WaitGroup
	wg.Add(1)
	firstErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		firstErr <- run(nil, "0.1.1", "sfu", new(bytes.Buffer), &testHooks{
			releaseURL:     srv.URL + "/releases/latest",
			executablePath: hooks.executablePath,
		})
	}()

	// Wait until the first updater is inside its apply phase (lock held),
	// then start the rival.
	<-entered
	secondErr := make(chan error, 1)
	go func() {
		secondErr <- run(nil, "0.1.1", "sfs", new(bytes.Buffer), &testHooks{
			releaseURL:     srv.URL + "/releases/latest",
			executablePath: hooks.executablePath,
		})
	}()

	secondResult := <-secondErr
	close(leave)
	wg.Wait()
	if err := <-firstErr; err != nil {
		t.Fatalf("first updater failed: %v", err)
	}
	if secondResult == nil {
		t.Fatal("second updater succeeded while the first held the lock, want clean failure")
	}
	if !strings.Contains(secondResult.Error(), "another update is in progress") {
		t.Fatalf("second updater err = %v, want in-progress message", secondResult)
	}
	// Both updaters share the same dir/hooks; the first must have applied
	// every pending target exactly once.
	if len(applied) != 2 {
		t.Fatalf("applied = %d payloads, want 2 (only the first updater applies)", len(applied))
	}
	// The winner's deferred release removed the lock.
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); !os.IsNotExist(err) {
		t.Fatalf("lock still present after first updater finished, err=%v", err)
	}
}

// TestParseLockFileDegradesGracefully covers malformed lock content.
func TestParseLockFileDegradesGracefully(t *testing.T) {
	h := parseLockFile([]byte("not a lock"))
	if h.pid != 0 || !h.started.IsZero() {
		t.Fatalf("parseLockFile(garbage) = %+v, want zero fields", h)
	}
	h = parseLockFile([]byte("pid=42\nstarted=" + time.Now().UTC().Format(time.RFC3339) + "\n"))
	if h.pid != 42 || h.started.IsZero() {
		t.Fatalf("parseLockFile = %+v, want pid 42 and a timestamp", h)
	}
	h = parseLockFile([]byte("pid=42\nstarted=" + time.Now().UTC().Format(time.RFC3339) + "\nhost=build-box\n"))
	if h.host != "build-box" {
		t.Fatalf("parseLockFile = %+v, want host build-box", h)
	}
}

// reapedProcessPID returns the pid of a child process that has been spawned
// and reaped, so it is (absent pid reuse) provably dead on this host.
func reapedProcessPID(t *testing.T) int {
	t.Helper()
	// "-test.list=^$" runs the test binary's flag parsing and exits without
	// running any test: a portable, always-present executable.
	cmd := exec.Command(os.Args[0], "-test.list=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn probe child: %v", err)
	}
	return cmd.Process.Pid
}

// TestUpdateLockSparesLiveHolder is the H-13 regression: a lock whose holder
// pid is demonstrably live on this host must never be stolen, however old the
// recorded start time is. Age alone used to decide staleness, so a slow
// updater (long serial payload downloads) had its live lock stolen after 30
// minutes and a second updater ran concurrently.
func TestUpdateLockSparesLiveHolder(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)
	old := time.Now().Add(-2 * lockStaleAge).UTC().Format(time.RFC3339)
	content := fmt.Sprintf("pid=%d\nstarted=%s\nhost=%s\n", os.Getpid(), old, hostnameForLock())
	if err := os.WriteFile(lockPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireUpdateLock(dir)
	if err == nil {
		release()
		t.Fatalf("live holder's %d-minute-old lock was stolen", 2*30)
	}
	if _, isHeld := lockHeldError(err); !isHeld {
		t.Fatalf("err = %v, want held-lock error", err)
	}
	// The loser must not have clobbered the holder's lock.
	got, readErr := os.ReadFile(lockPath)
	if readErr != nil {
		t.Fatalf("holder lock vanished: %v", readErr)
	}
	if string(got) != content {
		t.Fatalf("lock content changed: %q -> %q", content, got)
	}
}

// TestUpdateLockStealsDeadHolder proves the stale-steal still fires for a
// genuinely crashed holder: provably dead pid plus an old start time.
func TestUpdateLockStealsDeadHolder(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)
	old := time.Now().Add(-2 * lockStaleAge).UTC().Format(time.RFC3339)
	content := fmt.Sprintf("pid=%d\nstarted=%s\nhost=%s\n", reapedProcessPID(t), old, hostnameForLock())
	if err := os.WriteFile(lockPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("dead holder's stale lock not stolen: %v", err)
	}
	defer release()
	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), fmt.Sprintf("pid=%d", 0)) {
		t.Fatalf("lock = %q", got)
	}
	if strings.Contains(string(got), content[:strings.Index(content, "\n")+1]) {
		t.Fatalf("lock = %q, want a rewritten file with the new pid", got)
	}
}

// TestUpdateLockSparesForeignHostHolder proves the hostname guard: a lock
// recorded by another machine carries a pid that is meaningless here, so the
// local probe can never prove it dead and the lock is never stolen (the
// operator deletes it by hand, per the held-lock advice).
func TestUpdateLockSparesForeignHostHolder(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)
	old := time.Now().Add(-2 * lockStaleAge).UTC().Format(time.RFC3339)
	foreign := "some-other-update-host.invalid"
	content := fmt.Sprintf("pid=%d\nstarted=%s\nhost=%s\n", os.Getpid(), old, foreign)
	if err := os.WriteFile(lockPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := acquireUpdateLock(dir)
	if err == nil {
		release()
		t.Fatal("foreign-host lock was stolen via a local pid probe")
	}
	if _, isHeld := lockHeldError(err); !isHeld {
		t.Fatalf("err = %v, want held-lock error", err)
	}
}

// TestUpdateLockStealSparesFreshReplacement is the regression test for the
// steal TOCTOU: a stealer that observed the lock as stale must not remove a
// fresh lock that a rival stealer installed between the observation and the
// steal. The identity re-check must detect the replacement, loop back, and
// fail with the held-lock error while leaving the rival's lock untouched.
func TestUpdateLockStealSparesFreshReplacement(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)
	stale := "pid=999999\nstarted=" + time.Now().Add(-2*lockStaleAge).UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(lockPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := "pid=424242\nstarted=" + time.Now().UTC().Format(time.RFC3339) + "\n"

	// The rival wins the race in the window between our read of the stale
	// lock and our remove: it discards the stale lock and installs its
	// own fresh one.
	beforeStealRemove = func(_ string) {
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			t.Fatalf("rival remove: %v", err)
		}
		if err := os.WriteFile(lockPath, []byte(fresh), 0o644); err != nil {
			t.Fatalf("rival fresh write: %v", err)
		}
	}
	t.Cleanup(func() { beforeStealRemove = nil })

	_, err := acquireUpdateLock(dir)
	if err == nil {
		t.Fatal("acquire succeeded after a rival replaced the stale lock, want held-lock error")
	}
	if !strings.Contains(err.Error(), "another update is in progress") {
		t.Fatalf("err = %v, want in-progress message for the rival's fresh lock", err)
	}

	// The rival's fresh lock must survive untouched: a stealer never
	// destroys a lock whose content it did not validate.
	got, rerr := os.ReadFile(lockPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != fresh {
		t.Fatalf("fresh lock destroyed/rewritten by the stealer: %q -> %q", fresh, got)
	}
}

// TestUpdateLockOrphanedContentGrace covers the create-crash window: a lock
// with no parseable pid=/started= content is stealable after a short
// orphanedLockGrace instead of blocking the next run for the full
// lockStaleAge, but stays held while the grace window is still open.
func TestUpdateLockOrphanedContentGrace(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, lockFileName)
	backdate := func(age time.Duration) {
		t.Helper()
		mt := time.Now().Add(-age)
		if err := os.Chtimes(lockPath, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	// Empty lock from a crash between O_EXCL create and the content
	// write, older than the grace window: stealable.
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(20 * time.Second)
	release, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("orphaned empty lock not stolen after the grace window: %v", err)
	}
	release()

	// Inside the grace window: held.
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(5 * time.Second)
	_, err = acquireUpdateLock(dir)
	if err == nil {
		t.Fatal("acquire stole an orphaned lock inside the grace window, want held error")
	}
	if _, isHeld := lockHeldError(err); !isHeld {
		t.Fatalf("err = %v, want held-lock error", err)
	}

	// Unparseable junk behaves like an empty lock: short grace, not the
	// full staleness age.
	if err := os.WriteFile(lockPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(20 * time.Second)
	release, err = acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("orphaned junk lock not stolen after the grace window: %v", err)
	}
	release()
}

// --- P4-W1: identity-checked release with owner tokens ---

// TestParseLockFileOwnerToken covers owner parsing and its absence in
// legacy locks (pure content, runs on every platform).
func TestParseLockFileOwnerToken(t *testing.T) {
	started := time.Now().UTC().Format(time.RFC3339)
	h := parseLockFile([]byte("pid=42\nstarted=" + started + "\nowner=0123456789abcdef0123456789abcdef\n"))
	if h.owner != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("owner = %q, want parsed token", h.owner)
	}
	if h.pid != 42 || h.started.IsZero() {
		t.Fatalf("pid/started = %d/%v, want parsed", h.pid, h.started)
	}
	legacy := parseLockFile([]byte("pid=42\nstarted=" + started + "\n"))
	if legacy.owner != "" {
		t.Fatalf("legacy lock owner = %q, want empty", legacy.owner)
	}
}

// TestAcquireUpdateLockWritesOwnerToken proves every newly-created lock
// carries a 128-bit owner token, and that two acquisitions never share one.
func TestAcquireUpdateLockWritesOwnerToken(t *testing.T) {
	token := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		release, err := acquireUpdateLock(dir)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		t.Cleanup(release)
		data, err := os.ReadFile(filepath.Join(dir, lockFileName))
		if err != nil {
			t.Fatal(err)
		}
		h := parseLockFile(data)
		if len(h.owner) != 32 {
			t.Fatalf("owner token %q, want 32 hex chars", h.owner)
		}
		if _, err := hex.DecodeString(h.owner); err != nil {
			t.Fatalf("owner token %q is not hex: %v", h.owner, err)
		}
		return h.owner
	}
	first, second := token(t), token(t)
	if first == second {
		t.Fatalf("two acquisitions share owner token %q, want random tokens", first)
	}
}

// TestReleaseSparesRivalReplacement is the RR-4.1 regression: a suspended
// updater A wakes after rival B stole its stale lock and replaced the file.
// A's deferred release must leave B's lock intact — an unconditional remove
// would let a third updater run concurrently with B.
func TestReleaseSparesRivalReplacement(t *testing.T) {
	dir := t.TempDir()
	releaseA, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	lockPath := filepath.Join(dir, lockFileName)

	// B steals A's lock: fresh file, fresh inode, fresh token.
	bContent := "pid=777\nstarted=" + time.Now().UTC().Format(time.RFC3339) + "\nowner=ffffffffffffffffffffffffffffffff\n"
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(bContent), 0o644); err != nil {
		t.Fatal(err)
	}

	releaseA() // must be a no-op now

	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("B's lock removed by A's release: %v", err)
	}
	if string(got) != bContent {
		t.Fatalf("B's lock rewritten: %q -> %q", bContent, got)
	}
}

// TestReleaseSparesIdentityReplacement proves the file-identity half of the
// release check: a fresh file carrying even the identical content (same
// owner token) is a different file and must not be removed. The replacement
// pins a different mtime explicitly: some file systems hand a remove+create
// replacement the freed inode AND a same-tick coarse timestamp, so the
// mtime token is the only observable difference. On Windows the identity
// token IS the content, so identical content is the same logical lock —
// covered by the pure content tests instead.
func TestReleaseSparesIdentityReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("identity falls back to content on Windows; identical content is the same logical lock")
	}
	dir := t.TempDir()
	releaseA, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lockPath := filepath.Join(dir, lockFileName)
	content, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	// Recreate with byte-identical content but a distinct mtime: new inode
	// (where the file system does not reuse the freed one), same token.
	if err := os.WriteFile(lockPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lockPath, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	releaseA()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("identity-replaced lock removed by release: %v", err)
	}
}

// TestReleaseVanishedLockIsNoop proves a release of an already-gone lock
// neither errors nor recreates anything.
func TestReleaseVanishedLockIsNoop(t *testing.T) {
	dir := t.TempDir()
	releaseA, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, lockFileName)); err != nil {
		t.Fatal(err)
	}
	releaseA()
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); !os.IsNotExist(err) {
		t.Fatalf("release resurrected the lock, err=%v", err)
	}
}

// TestLockIdentityContentFallback is the pure Windows token-fallback check:
// with no inode, identity is decided by content bytes, then mtime+size.
func TestLockIdentityContentFallback(t *testing.T) {
	a := lockIdentity{content: "pid=1\nowner=aa"}
	same := lockIdentity{content: "pid=1\nowner=aa"}
	other := lockIdentity{content: "pid=2\nowner=bb"}
	if !a.sameAs(same) {
		t.Fatal("identical content reported as different lock")
	}
	if a.sameAs(other) {
		t.Fatal("different content reported as the same lock")
	}
	// The inode token is authoritative whenever it is available, even
	// against identical content.
	inode := lockIdentity{ino: 7, content: "x"}
	if inode.sameAs(lockIdentity{ino: 8, content: "x"}) {
		t.Fatal("inode token ignored in favor of content")
	}
	// With neither inode nor content, mtime+size is the last resort.
	mt := time.Unix(1000, 0).UTC()
	m1 := lockIdentity{modTime: mt, size: 5}
	if !m1.sameAs(lockIdentity{modTime: mt, size: 5}) {
		t.Fatal("equal mtime+size reported as different")
	}
	if m1.sameAs(lockIdentity{modTime: mt, size: 6}) {
		t.Fatal("different size reported as same")
	}
}
