package selfupdate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// lockFileName guards the apply phase of an update: one updater at a time per
// install dir. Two binaries running `update` concurrently would otherwise both
// download and swap every sibling; on Windows the concurrent rename-aside
// replacement of the same target can collide (see journal.go, and the
// dependency's <name>.old scheme it replaced).
const lockFileName = ".snowfast-update.lock"

// lockStaleAge bounds how long a crashed updater's lock blocks updates.
// Payload downloads are capped at 64 MiB and applies are local renames, so a
// live update finishes in minutes; 30 minutes comfortably exceeds any real
// run while still recovering quickly from a SIGKILL or power loss.
const lockStaleAge = 30 * time.Minute

// orphanedLockGrace bounds how long a lock with no parseable pid= or
// started= content is trusted: such a lock is an orphan from a crash
// between the O_EXCL create and the content write (or foreign junk), and it
// carries no evidence of a live holder, so letting it block the next run
// for the full lockStaleAge on the mtime fallback is disproportionate. A
// live updater always writes its content within milliseconds of creating
// the file, so this grace only ever delays a genuinely orphaned file.
const orphanedLockGrace = 15 * time.Second

// lockWaitHint is the user-facing recovery advice appended to the held-lock
// error when another updater is still running.
const lockWaitHint = "re-run once the other update finishes, or delete the lock file if no other update is running"

// heldLock describes an existing lock we did not acquire.
type heldLock struct {
	pid     int
	started time.Time
	owner   string // random 128-bit token; empty on legacy pid/started locks
	host    string // holder hostname; empty on legacy locks (treated as local)
}

// errLockHeld reports a live lock held by another updater.
type errLockHeld struct{ held heldLock }

func (e *errLockHeld) Error() string {
	age := ""
	if !e.held.started.IsZero() {
		age = fmt.Sprintf(", started %s", e.held.started.Format(time.RFC3339))
	}
	return fmt.Sprintf("another update is in progress (pid %d%s) — %s", e.held.pid, age, lockWaitHint)
}

// parseLockFile extracts pid, timestamp and owner token from lock file
// content ("pid=<n>\nstarted=<RFC3339>\nowner=<32 hex>\n"). Missing or
// unparseable fields degrade gracefully: pid 0, zero time, empty owner (a
// legacy lock without an owner= line — or a lock with neither pid= nor
// started= parseable — is judged stale by the rules in readLockState). The
// owner token exists purely for identity-checked release (see
// releaseOwnedLock); stale stealing never trusts it.
func parseLockFile(data []byte) heldLock {
	var h heldLock
	for _, line := range strings.Split(string(data), "\n") {
		if val, ok := strings.CutPrefix(strings.TrimSpace(line), "pid="); ok {
			if n, err := strconv.Atoi(val); err == nil {
				h.pid = n
			}
		}
		if val, ok := strings.CutPrefix(strings.TrimSpace(line), "started="); ok {
			if t, err := time.Parse(time.RFC3339, val); err == nil {
				h.started = t
			}
		}
		if val, ok := strings.CutPrefix(strings.TrimSpace(line), "owner="); ok {
			h.owner = val
		}
		if val, ok := strings.CutPrefix(strings.TrimSpace(line), "host="); ok {
			h.host = val
		}
	}
	return h
}

// newLockOwnerToken returns a random 128-bit owner token as 32 lowercase
// hex chars. Two live updaters can never share a token, so a release that
// observes a different token knows the file is someone else's.
func newLockOwnerToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// lockIdentity is the identity token a stealer captures when it reads the
// lock content in readLockState, and re-checks immediately before removing
// the file. Unix uses the file's inode; Windows (where os.FileInfo carries
// no inode) falls back to the tiny file's content bytes, then to
// mtime+size. The re-check shrinks the steal race to the stat→remove gap
// and, crucially, guarantees a stealer never removes a lock whose content
// it did not observe: a rival that replaced the stale lock with a fresh one
// in between changes the inode (new file) on unix and the content/mtime on
// Windows, so the stale observation cannot destroy the fresh holder's lock.
// No token is trusted alone — see sameAs for why all observed tokens must
// agree.
type lockIdentity struct {
	ino     uint64    // inode; 0 when unavailable (Windows)
	modTime time.Time // mtime+size: last-resort token
	size    int64
	content string // file bytes; "" when unreadable
}

// sameAs reports whether the lock on disk is still the file this identity
// was captured from, judged by agreement of every token BOTH sides observed.
// No single token is authoritative: inode numbers are reused by some
// file systems (a remove+create replacement can be handed the freed inode,
// making an inode-only match indistinguishable from the original file), and
// a replacement lock carries different content (its own owner token) unless
// hand-crafted, while a same-content recreation differs in mtime. Requiring
// every observed token to agree keeps a stale-observation or suspended
// updater from destroying a file it did not create.
func (id lockIdentity) sameAs(cur lockIdentity) bool {
	if id.ino != 0 && cur.ino != 0 && id.ino != cur.ino {
		return false
	}
	if id.content != "" && cur.content != "" && id.content != cur.content {
		return false
	}
	if !id.modTime.IsZero() && !cur.modTime.IsZero() &&
		(!id.modTime.Equal(cur.modTime) || id.size != cur.size) {
		return false
	}
	return true
}

// statLockIdentity derives the identity token from a stat of the lock plus
// the content observed at read time ("" when the content was unreadable).
func statLockIdentity(info os.FileInfo, content string) lockIdentity {
	return lockIdentity{
		ino:     fileIDFromSys(info.Sys()),
		modTime: info.ModTime(),
		size:    info.Size(),
		content: content,
	}
}

// lockIdentityMatches re-observes the lock at path and reports whether it
// is still the same file the stealer read. A vanished or replaced file
// reports false so the caller loops back to the create/read decision
// instead of removing.
func lockIdentityMatches(path string, id lockIdentity) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	cur := statLockIdentity(info, "")
	// Re-read the content for the byte-level token even where the inode is
	// available: inode numbers are reused by some file systems, so a
	// replacement file can match on inode alone. A read failure keeps the
	// stat-derived tokens rather than failing the whole re-check.
	if data, rerr := os.ReadFile(path); rerr == nil {
		cur.content = string(data)
	}
	return id.sameAs(cur)
}

// acquireUpdateLock creates <dir>/.snowfast-update.lock exclusively. A fresh
// O_CREATE|O_EXCL file proves no other updater holds it; an existing file is
// only stolen when it is stale — older than lockStaleAge by its recorded
// start time, by file mtime when the content is unreadable/unparseable, or
// after orphanedLockGrace when the content carries no parseable pid/started
// at all. H-13: a lock whose recorded pid is demonstrably live on the
// holder's host is never stolen, however old it is; see holderRefusesSteal.
// Newly-created locks carry a random 128-bit owner= token and the host=
// hostname for the cross-machine liveness check (legacy pid/started-only
// locks keep the same stale-steal rules with a local-only probe). Before
// removing a stale lock the stealer re-checks the identity captured at read
// time (see lockIdentity) so a lock replaced by a rival after the read is
// never destroyed. The returned release func removes the file ONLY when it
// is still the file that was acquired — identity (inode, or content/mtime
// on Windows) and owner token both still matching — so a suspended updater
// that resumes after a rival stole its stale lock can never delete the
// rival's lock; missing or replaced files make release a no-op. Release must
// be deferred by the caller; a nil release with a non-nil error means the
// lock was not acquired.
func acquireUpdateLock(dir string) (release func(), err error) {
	path := filepath.Join(dir, lockFileName)
	owner, err := newLockOwnerToken()
	if err != nil {
		return nil, fmt.Errorf("generate update lock owner token: %w", err)
	}
	var content []byte
	content = append(content, []byte(fmt.Sprintf("pid=%d\nstarted=%s\nowner=%s\nhost=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339), owner, hostnameForLock()))...)

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := f.Write(content)
			cerr := f.Close()
			if werr != nil || cerr != nil {
				// Lock file with no/!partial content would look stale to the
				// next run; remove it so we do not leave a 30-min block.
				os.Remove(path)
				return nil, fmt.Errorf("write update lock: %w", errors.Join(werr, cerr))
			}
			// Capture the acquired file identity right after write/close:
			// the release closure removes the file only while it is still
			// provably ours (identity + owner token). Without a stat we
			// could not prove ownership later, so a vanished file at this
			// point fails the acquire rather than handing out a lock whose
			// release is unprovable.
			info, statErr := os.Stat(path)
			if statErr != nil {
				os.Remove(path)
				return nil, fmt.Errorf("stat fresh update lock: %w", statErr)
			}
			id := statLockIdentity(info, string(content))
			release = func() { releaseOwnedLock(path, id, owner) }
			return release, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, withPermHint(fmt.Errorf("create update lock: %w", err))
		}
		// Someone else holds the lock. Steal it only when stale.
		stale, holder, id, readErr := readLockState(path)
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				continue // the lock vanished between create and read; retry
			}
			return nil, fmt.Errorf("read update lock: %w", readErr)
		}
		if !stale {
			return nil, &errLockHeld{held: holder}
		}
		// Stale: remove and retry the exclusive create. Before removing,
		// require the file to still be the same lock we read: a rival
		// stealer may have removed the stale lock and installed its own
		// fresh one after our observation, and an unconditional remove
		// here would destroy the new holder's lock (the classic
		// read→remove TOCTOU). On identity mismatch or vanish, loop back
		// to the create/read decision. The stat→remove gap remains, but a
		// stealer can no longer destroy a lock whose content it never
		// validated.
		if beforeStealRemove != nil {
			beforeStealRemove(path)
		}
		if !lockIdentityMatches(path, id) {
			continue
		}
		if rmErr := os.Remove(path); rmErr != nil {
			if errors.Is(rmErr, fs.ErrNotExist) {
				continue // another updater stole it first; retry the create
			}
			return nil, fmt.Errorf("steal stale update lock: %w", rmErr)
		}
	}
}

// releaseOwnedLock removes the acquired lock only while it is still
// provably ours: the content must still carry our owner token (a replaced
// file carries the rival's token, or none for foreign junk), and the file
// must still be the same file the acquire captured (inode on unix;
// content/mtime tokens on Windows). A missing file is a no-op — someone
// else already removed it. This is the RR-4.1 fix: a suspended updater
// resuming after a rival stole its stale lock can no longer delete the
// rival's lock, which would let a third updater apply concurrently.
func releaseOwnedLock(path string, id lockIdentity, owner string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // vanished or unreadable: nothing safe to remove
	}
	if owner != "" && parseLockFile(data).owner != owner {
		return // the acquired file was replaced by another holder's lock
	}
	if !lockIdentityMatches(path, id) {
		return // replaced under us
	}
	os.Remove(path)
}

// hostnameForLock returns this machine's hostname for the lock file's host=
// line ("" when unavailable, e.g. a broken hostname syscall). A lock with no
// readable host= is treated as a legacy local lock, so an empty value here
// only degrades the cross-machine guard, never the local probe.
func hostnameForLock() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// holderRefusesSteal reports whether the recorded holder pid could still be
// live, in which case the H-13 stale-steal must not fire even past the
// staleness window. Stealing requires the pid to be PROVABLY dead on the
// holder's host: a live or undecided local pid refuses, and a host= recorded
// from another machine refuses outright — that pid is meaningless here, so
// no local probe can ever prove it dead (the held-lock error's advice tells
// the operator to re-run or delete the lock by hand). Locks with no pid= at
// all (orphaned content from a crash between create and write, or foreign
// junk) carry no owner evidence and keep the pre-H-13 grace rules.
func holderRefusesSteal(h heldLock) bool {
	if h.pid == 0 {
		return false
	}
	if h.host != "" && h.host != hostnameForLock() {
		return true
	}
	return !pidProvablyDead(h.pid)
}

// readLockState reports whether the lock at path is stale, plus the holder
// info for error messages and the file identity for the steal re-check. A
// lock with a parseable started= is stale after lockStaleAge on the holder's
// own clock — but only when the recorded pid is provably dead (H-13); a
// parseable pid without a usable timestamp is stale by mtime after
// lockStaleAge under the same liveness gate; content with no parseable pid=
// or started= at all (an orphan from a crash between create and content
// write) is stale by mtime after the short orphanedLockGrace; unreadable
// content (permissions) is stale by mtime after lockStaleAge. A missing file
// is not stale; it is gone, and the caller retries the create (reported via
// ErrNotExist).
func readLockState(path string) (stale bool, holder heldLock, id lockIdentity, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return false, heldLock{}, lockIdentity{}, readErr
		}
		// Unreadable content (permissions): fall back to mtime.
		return judgeLockByMtime(path, lockStaleAge)
	}
	holder = parseLockFile(data)
	switch {
	case !holder.started.IsZero():
		// Parseable started=: the holder's own clock decides. H-13: age
		// alone is no longer sufficient — a holder whose pid is
		// demonstrably live (slow serial payload downloads can exceed
		// lockStaleAge) keeps its lock; only a provably dead holder is
		// stale. Locks with no pid (or an unprovable one) keep the
		// age-only rules, which is also the legacy-lock behavior.
		info, statErr := os.Stat(path)
		if statErr != nil {
			return false, holder, lockIdentity{}, statErr
		}
		stale := time.Since(holder.started) > lockStaleAge
		if stale && holderRefusesSteal(holder) {
			return false, holder, statLockIdentity(info, string(data)), nil
		}
		return stale, holder, statLockIdentity(info, string(data)), nil
	case holder.pid != 0:
		// A pid we could read but no usable timestamp: no holder clock to
		// trust, so fall back to mtime over the full lockStaleAge. The
		// same H-13 liveness gate applies once the mtime window expires.
		stale, _, id, err := judgeLockByMtime(path, lockStaleAge)
		if err == nil && stale && holderRefusesSteal(holder) {
			return false, holder, id, nil
		}
		return stale, holder, id, err
	default:
		// No parseable pid= or started= at all: an orphaned lock from a
		// crash between the O_EXCL create and the content write (or
		// foreign junk). Trusting it for the full lockStaleAge would
		// block the next run on a file that carries no evidence of a live
		// holder, so only orphanedLockGrace applies.
		return judgeLockByMtime(path, orphanedLockGrace)
	}
}

// judgeLockByMtime decides staleness from the file mtime alone and returns
// the stat-derived identity for the steal re-check. A vanished lock is
// reported as not stale with an ErrNotExist error so the caller retries the
// create.
func judgeLockByMtime(path string, age time.Duration) (stale bool, holder heldLock, id lockIdentity, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return false, heldLock{}, lockIdentity{}, statErr
	}
	return time.Since(info.ModTime()) > age, heldLock{}, statLockIdentity(info, ""), nil
}

// lockHeldError extracts the held-lock detail when err is an errLockHeld.
func lockHeldError(err error) (*errLockHeld, bool) {
	var e *errLockHeld
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
