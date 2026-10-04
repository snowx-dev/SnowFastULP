package sflog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

// EnvCopyMaxLen is the per-file cap for a single -env copy (16 MiB).
// Exported so the TUI recap can name the cap from the real limit.
const EnvCopyMaxLen = 16 << 20 // 16 MiB

// TdataCopyMaxBytes is the whole-tree cap for a Telegram tdata copy. A tree
// that would exceed this is skipped (no partial dest, no -del). Tests may
// lower it.
var TdataCopyMaxBytes int64 = 5 << 30

var errTdataOverCap = errors.New("tdata folder exceeds 5 GiB copy limit")
var errEnvCopyOverCap = errors.New("env file exceeds copy size limit")

// envCopyBasenames is the high-confidence allowlist for -env file copy.
var envCopyBasenames = map[string]bool{
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	".netrc": true, ".pgpass": true, ".htpasswd": true,
	".git-credentials": true, ".npmrc": true, ".pypirc": true, ".dockercfg": true,
	".boto": true, ".s3cfg": true,
	"credentials": true, "credentials.json": true, "credentials.xml": true,
	"secrets.json": true, "secret.json": true,
	"apikeys.json": true, "api_keys.json": true,
	"service_account.json": true, "service-account.json": true,
	"token.json": true, "tokens.json": true, "auth.json": true,
	"wallet.dat": true, "seed.txt": true, "mnemonic.txt": true, "recovery.txt": true,
}

var envCopyExtensions = map[string]bool{
	".pem": true, ".key": true, ".ppk": true, ".p12": true, ".pfx": true,
	".kdbx": true, ".asc": true, ".ovpn": true, ".env": true, ".properties": true,
}

var envCopyConditionalExts = map[string]bool{
	".json": true, ".yaml": true, ".yml": true, ".ini": true,
	".toml": true, ".cfg": true, ".conf": true,
}

var envCopyBasenameTokens = []string{
	"credential", "secret", "apikey", "api_key", "token",
	"serviceaccount", "firebase", "appsettings",
}

// isEnvCopyCandidate reports whether path should be copied under -env.
func isEnvCopyCandidate(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if strings.HasSuffix(name, ".pub") {
		return false
	}
	if envCopyBasenames[name] {
		return true
	}
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return true
	}
	ext := filepath.Ext(name)
	if envCopyExtensions[ext] {
		return true
	}
	if envCopyConditionalExts[ext] {
		lower := strings.ToLower(strings.TrimSuffix(name, ext))
		for _, tok := range envCopyBasenameTokens {
			if strings.Contains(lower, tok) {
				return true
			}
		}
	}
	return false
}

// safeRelPath sanitizes a relative path for writing under a secrets/staging
// root. Absolute, UNC, and Windows volume prefixes (C:) are stripped so
// filepath.Join(root, rel) cannot discard root on Windows. ".." and empty
// parts are dropped; leftover drive-like elements containing ':' are skipped.
func safeRelPath(rel string) string {
	s := strings.ReplaceAll(rel, "\\", "/")
	for strings.HasPrefix(s, "/") {
		s = s[1:]
	}
	parts := strings.Split(s, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			continue
		}
		if strings.Contains(p, ":") {
			continue
		}
		out = append(out, sanitizePathElem(p))
	}
	if len(out) == 0 {
		return "_"
	}
	return filepath.Join(out...)
}

// destUnderRoot reports whether dest is root or a path inside it. Both are
// cleaned; a trailing separator on root prevents /secrets matching /secrets-evil.
func destUnderRoot(root, dest string) bool {
	root = filepath.Clean(root)
	dest = filepath.Clean(dest)
	if dest == root {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	return strings.HasPrefix(dest, root)
}

func sanitizePathElem(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == 0 {
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

type EnvCopyErrorKind string

const (
	EnvCopyOpenError      EnvCopyErrorKind = "open"
	EnvCopyReadError      EnvCopyErrorKind = "read"
	EnvCopyWriteError     EnvCopyErrorKind = "write"
	EnvCopyCollisionError EnvCopyErrorKind = "collision"
	EnvCopyTdataError     EnvCopyErrorKind = "tdata"
	EnvCopyOverCap        EnvCopyErrorKind = "over-cap"
)

// EnvCopyIssue is a categorized, provenance-safe -env copy failure.
// Path identifies the source file or archive member, never its contents.
type EnvCopyIssue struct {
	Path string
	Kind EnvCopyErrorKind
	Err  error
}

// EnvCopyStats holds final -env copy counters.
type EnvCopyStats struct {
	Copied, SkippedOverCap, WriteErrors                                 int
	OpenErrors, ReadErrors, WriteFailures, CollisionErrors, TdataErrors int
	Issues                                                              []EnvCopyIssue
	// Deduped counts byte-identical flat env/key files whose copy was skipped
	// because an identical payload was already retained this run. A duplicate
	// is a successful outcome, not an issue: the source stays -del eligible.
	Deduped int
	// DirsCopied counts Telegram tdata folders copied whole (loose or promoted
	// from archive staging). Copied counts only flat env/key files.
	DirsCopied int
	// DirsSkippedOverCap counts tdata folders skipped for exceeding
	// TdataCopyMaxBytes. Distinct from SkippedOverCap (flat env/key files).
	DirsSkippedOverCap int
}

type envJob struct {
	srcPath    string // loose on-disk file (empty for archive members)
	data       []byte // in-memory bytes (archive members)
	memberName string // in-archive path (archive members)
	issuePath  string // source path or source!member provenance
}

// envFingerprint records one retained flat env file: its size and final path.
// Keyed by xxhash64 in EnvCopier.fingerprints; equal hashes are still
// byte-compared in writeJob, so a forced or natural collision lands as a
// distinct output rather than being dropped as a duplicate.
type envFingerprint struct {
	size int64
	path string
}

// newEnvHasher is a package-level seam so tests can inject a deterministic
// hasher and force hash collisions.
var newEnvHasher = func() hash.Hash64 { return xxhash.New() }

// osRemove is a package-level seam so tests can inject removal failures in
// the dedup path.
var osRemove = os.Remove

// EnvCopier asynchronously copies env/key files flat into root
// (<out>/sfl_<stamp>_secrets/).
type EnvCopier struct {
	root    string
	prog    *Progress
	maxLen  int64
	queue   chan envJob
	wg      sync.WaitGroup
	started atomic.Bool

	// ctx/cancel are self-owned: cancellation stops enqueue acceptance (a full
	// queue can never block shutdown) and Close cancels before closing the
	// queue, so a send can never hit a closed channel. closeOnce makes close
	// state explicit and idempotent.
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	// sendMu serializes enqueue-vs-close: sends happen only while closed is
	// false, and Close flips it under the same lock before closing the queue.
	sendMu sync.Mutex
	closed bool

	mu      sync.Mutex
	stats   EnvCopyStats
	onError func(EnvCopyIssue)
	// runErr records lifecycle run errors (enqueue raced shutdown or the
	// engine cancelled the run with the queue full). Deliberately not a copy
	// issue: it never enters stats.Issues; callers surface it via CallbackError.
	runErr error
	// fingerprints maps xxhash64 -> retained files with that hash. Owned by
	// the single copy worker goroutine (writeJob); no lock is needed.
	fingerprints map[uint64][]envFingerprint
	// cb guards the integration error-handler callback against panics: a
	// panicking hook becomes a recorded run error (CallbackError), not a crash.
	cb *callbackGuard
	// dirMu guards destination directory naming for recursive tdata copies
	// (CopyDir / PromoteTdata) since multiple archive workers may finish
	// confirmed tdata trees concurrently. The file-copy path is already
	// serialized on the single worker goroutine.
	dirMu sync.Mutex
}

// NewEnvCopier creates a copier writing flat into root. The directory is
// created lazily on the first successful write so an empty run leaves nothing
// behind.
func NewEnvCopier(root string, prog *Progress, maxLen int64) *EnvCopier {
	if maxLen <= 0 {
		maxLen = EnvCopyMaxLen
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &EnvCopier{
		root:         root,
		prog:         prog,
		maxLen:       maxLen,
		queue:        make(chan envJob, 256),
		fingerprints: make(map[uint64][]envFingerprint),
		ctx:          ctx,
		cancel:       cancel,
		cb:           newCallbackGuard(cancel),
	}
}

// SetErrorHandler receives every categorized copy failure. The callback must
// be safe for concurrent use because archive workers can report failures while
// the copier worker reports destination errors.
func (c *EnvCopier) SetErrorHandler(fn func(EnvCopyIssue)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onError = fn
	c.mu.Unlock()
}

// Start launches the background copy worker. Idempotent.
func (c *EnvCopier) Start() {
	if c == nil {
		return
	}
	c.ensureStarted()
}

// ensureStarted launches the background copy worker exactly once. Enqueue
// paths call it so void call sites (cmd never calls Start explicitly) work
// without a 257th-job deadlock on the fixed 256-slot queue.
func (c *EnvCopier) ensureStarted() {
	if c.started.Swap(true) {
		return
	}
	c.wg.Add(1)
	go c.worker()
}

// enqueue hands job to the copy worker, lazily starting it. The send is
// guarded by sendMu against Close (which flips closed under the same lock
// before closing the queue), and selects against ctx so a full queue can never
// block shutdown: cancellation is recorded as a run error, not a copy issue.
func (c *EnvCopier) enqueue(job envJob) {
	if c == nil {
		return
	}
	c.ensureStarted()
	c.sendMu.Lock()
	if c.closed {
		c.sendMu.Unlock()
		c.recordRunError(fmt.Errorf("env copy enqueue after close: %w", c.ctx.Err()))
		return
	}
	// Deterministic fast path: once cancelled, never accept new work (a bare
	// select could still win the send while the queue has free slots).
	if err := c.ctx.Err(); err != nil {
		c.sendMu.Unlock()
		c.recordRunError(fmt.Errorf("env copy enqueue cancelled: %w", err))
		return
	}
	select {
	case c.queue <- job:
		c.sendMu.Unlock()
	case <-c.ctx.Done():
		c.sendMu.Unlock()
		c.recordRunError(fmt.Errorf("env copy enqueue cancelled: %w", c.ctx.Err()))
	}
}

// cancelRun cancels the copier context (idempotent): pending enqueues stop
// blocking and late enqueues are rejected as run errors. Close still drains
// every accepted job.
func (c *EnvCopier) cancelRun() {
	if c == nil {
		return
	}
	c.cancel()
}

// recordRunError records the first lifecycle run error.
func (c *EnvCopier) recordRunError(err error) {
	c.mu.Lock()
	if c.runErr == nil {
		c.runErr = err
	}
	c.mu.Unlock()
}

// EnqueueFile queues a loose on-disk file for async copy.
func (c *EnvCopier) EnqueueFile(srcPath string) {
	if c == nil {
		return
	}
	info, err := os.Lstat(srcPath)
	if err != nil {
		c.recordError(EnvCopyOpenError, srcPath, err)
		return
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		c.recordError(EnvCopyOpenError, srcPath, os.ErrInvalid)
		return
	}
	if info.Size() > c.maxLen {
		c.recordSkippedOverCap(srcPath)
		return
	}
	c.enqueue(envJob{srcPath: srcPath, issuePath: srcPath})
}

func (c *EnvCopier) enqueueEnvBytes(issuePath, memberName string, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}
	if int64(len(data)) > c.maxLen {
		c.recordSkippedOverCap(issuePath)
		return
	}
	c.enqueue(envJob{memberName: memberName, data: data, issuePath: issuePath})
}

// CopyDir copies a loose tdata tree into <root>/tdata/ (with _2/_3 suffixes)
// synchronously so the caller can set -del eligibility from the outcome.
func (c *EnvCopier) CopyDir(srcDir string) error {
	if c == nil {
		return os.ErrInvalid
	}
	dest, err := c.reserveTdataDest()
	if err != nil {
		c.recordError(EnvCopyTdataError, srcDir, err)
		c.removeRootIfEmpty()
		return err
	}
	var used int64
	if _, err := copyTree(srcDir, dest, &used); err != nil {
		if errors.Is(err, errTdataOverCap) {
			c.bumpSkippedTdataOverCap()
		} else {
			c.recordError(EnvCopyTdataError, srcDir, err)
		}
		_ = os.RemoveAll(dest)
		c.removeRootIfEmpty()
		return err
	}
	c.creditTdataDir()
	return nil
}

// PromoteTdata moves a staged tdata tree into <root>/tdata/ (or tdata_2, …).
// Rename is tried first; any failure (EXDEV, Windows dest-exists) falls back
// to copyTree into the reserved dest, then the staging tree is removed.
func (c *EnvCopier) PromoteTdata(stagedDir string) error {
	if c == nil {
		return os.ErrInvalid
	}
	dest, err := c.reserveTdataDest()
	if err != nil {
		c.recordError(EnvCopyTdataError, stagedDir, err)
		c.removeRootIfEmpty()
		return err
	}
	if err := os.Rename(stagedDir, dest); err != nil {
		_, copyErr := copyTree(stagedDir, dest, new(int64))
		_ = os.RemoveAll(stagedDir)
		if copyErr != nil {
			if errors.Is(copyErr, errTdataOverCap) {
				c.bumpSkippedTdataOverCap()
			} else {
				c.recordError(EnvCopyTdataError, stagedDir, copyErr)
			}
			_ = os.RemoveAll(dest)
			c.removeRootIfEmpty()
			return copyErr
		}
		c.creditTdataDir()
		return nil
	}
	c.creditTdataDir()
	return nil
}

// reserveTdataDest claims a unique tdata / tdata_N directory under c.root by
// Mkdir (atomic) while holding dirMu, so concurrent PromoteTdata/CopyDir
// cannot pick the same name. The dest is 0700. Caller owns filling or
// removing it.
func (c *EnvCopier) reserveTdataDest() (string, error) {
	if err := os.MkdirAll(c.root, 0o700); err != nil {
		return "", err
	}
	c.dirMu.Lock()
	defer c.dirMu.Unlock()
	base := filepath.Join(c.root, "tdata")
	if err := os.Mkdir(base, 0o700); err == nil {
		return base, nil
	} else if !os.IsExist(err) {
		return "", err
	}
	for i := 2; ; i++ {
		candidate := base + "_" + itoa(i)
		err := os.Mkdir(candidate, 0o700)
		if err == nil {
			return candidate, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
}

// creditTdataDir counts a copied tdata tree. Tree internals are opaque session
// blobs, not env files: they never ride the live Env counter (prog.envCopied),
// which tracks flat env/key copies only. The recap names trees as a
// "N tdata folders" tail instead.
func (c *EnvCopier) creditTdataDir() {
	c.mu.Lock()
	c.stats.DirsCopied++
	c.mu.Unlock()
}

// CopyMember reads up to maxLen from r and enqueues when name is a candidate.
// Returns true only when bytes were queued for copy. Read errors do not enqueue
// partial data and are classified separately from destination write failures.
func (c *EnvCopier) CopyMember(ctx context.Context, issuePath, memberName string, r io.Reader) bool {
	if c == nil || !isEnvCopyCandidate(memberName) {
		return false
	}
	max := c.maxLen
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	drainErr := drainContext(ctx, r)
	if err != nil {
		c.recordError(EnvCopyReadError, issuePath+"!"+memberName, err)
		return false
	}
	if drainErr != nil {
		c.recordError(EnvCopyReadError, issuePath+"!"+memberName, drainErr)
		return false
	}
	if len(data) == 0 {
		return false
	}
	if int64(len(data)) > max {
		c.recordSkippedOverCap(issuePath + "!" + memberName)
		return false
	}
	c.enqueueEnvBytes(issuePath+"!"+memberName, memberName, data)
	return true
}

func (c *EnvCopier) worker() {
	defer c.wg.Done()
	for job := range c.queue {
		c.writeJob(job)
	}
}

func flatBasename(relDest string) string {
	base := filepath.Base(safeRelPath(relDest))
	if base == "" || base == "." {
		return "_"
	}
	return base
}

func (c *EnvCopier) writeJob(job envJob) {
	if c.fingerprints == nil {
		c.fingerprints = make(map[uint64][]envFingerprint)
	}
	var base string
	if job.srcPath != "" {
		base = sanitizePathElem(filepath.Base(job.srcPath))
	} else {
		base = flatBasename(job.memberName)
	}
	// Create the destination dir only when we are about to write. A single
	// worker serializes jobs, so concurrent mkdir races are not a concern.
	// filepath.Join keeps this correct on Windows and Unix.
	if err := os.MkdirAll(c.root, 0o700); err != nil {
		c.recordError(EnvCopyWriteError, job.issuePath, err)
		return
	}
	// Stage the payload into a 0600 temp file inside root while hashing it, so
	// exact-content dedup costs one source read and the retained representative
	// is moved into place by rename, never a second copy.
	staged, sum, size, err := c.stageJob(job)
	if err != nil {
		return // already recorded; staging file already removed
	}
	if c.isDuplicate(staged, sum, size, job.issuePath) {
		// Byte-identical payload already retained: a successful copy outcome
		// that is not a Copied file and not an issue, so the source stays
		// -del eligible. The root cannot be empty here (a representative is
		// retained on disk).
		if rerr := osRemove(staged); rerr != nil {
			// A stranded .sfl-env-*.tmp in the secrets dir is a destination
			// write failure: record it (the issue marks the source HadIssue
			// so -del retains it) and withhold the dedup credit.
			c.recordError(EnvCopyWriteError, job.issuePath, rerr)
			return
		}
		c.mu.Lock()
		c.stats.Deduped++
		c.mu.Unlock()
		if c.prog != nil {
			c.prog.addEnvDeduped()
		}
		return
	}
	dest, ok := uniquePath(filepath.Join(c.root, base))
	if !ok {
		c.recordError(EnvCopyCollisionError, job.issuePath, os.ErrExist)
		_ = os.Remove(staged)
		c.removeRootIfEmpty()
		return
	}
	// Staging and destination share c.root, so rename is same-filesystem by
	// construction. A failure is a destination-side write failure; never fall
	// back to a second copy of the source.
	if err := os.Rename(staged, dest); err != nil {
		c.recordError(EnvCopyWriteError, job.issuePath, err)
		_ = os.Remove(staged)
		c.removeRootIfEmpty()
		return
	}
	c.fingerprints[sum] = append(c.fingerprints[sum], envFingerprint{size: size, path: dest})
	c.mu.Lock()
	c.stats.Copied++
	c.mu.Unlock()
	if c.prog != nil {
		c.prog.addEnvCopied(1)
	}
}

// stageJob streams the job payload into a 0600 temp file under root while
// hashing it, enforcing the size cap on the way through. On failure it removes
// the temp file, records the corresponding issue against the current source,
// and returns a non-nil error — the caller must not re-read the source. A
// successful stage closes the file before comparison or rename and returns its
// path, xxhash64, and byte size.
func (c *EnvCopier) stageJob(job envJob) (path string, sum uint64, size int64, err error) {
	f, cerr := os.CreateTemp(c.root, ".sfl-env-*.tmp")
	if cerr != nil {
		c.recordError(EnvCopyWriteError, job.issuePath, cerr)
		c.removeRootIfEmpty()
		return "", 0, 0, cerr
	}
	path = f.Name()
	drop := func(kind EnvCopyErrorKind, e error) (string, uint64, int64, error) {
		_ = f.Close()
		_ = os.Remove(path)
		c.recordError(kind, job.issuePath, e)
		c.removeRootIfEmpty()
		return "", 0, 0, e
	}
	skipOverCap := func() (string, uint64, int64, error) {
		_ = f.Close()
		_ = os.Remove(path)
		c.recordSkippedOverCap(job.issuePath)
		c.removeRootIfEmpty()
		return "", 0, 0, errEnvCopyOverCap
	}
	h := newEnvHasher()
	var n int64
	if job.srcPath != "" {
		in, oerr := openReadNoFollow(job.srcPath)
		if oerr != nil {
			return drop(EnvCopyWriteError, oerr)
		}
		defer in.Close() //nolint:errcheck // read-only source
		info, serr := in.Stat()
		if serr != nil || !info.Mode().IsRegular() {
			return drop(EnvCopyWriteError, os.ErrInvalid)
		}
		if info.Size() > c.maxLen {
			// The source grew (or was swapped) past maxLen after EnqueueFile's
			// pre-check; a policy cap skip, not a write failure.
			return skipOverCap()
		}
		// A mid-stream failure is classified against the source (read), not
		// the local temp file (write), so analysts see which side broke.
		n, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(in, c.maxLen+1))
		if err != nil {
			return drop(EnvCopyReadError, err)
		}
	} else {
		n, err = io.Copy(io.MultiWriter(f, h), bytes.NewReader(job.data))
		if err != nil {
			return drop(EnvCopyWriteError, err)
		}
	}
	if n > c.maxLen {
		return skipOverCap()
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		c.recordError(EnvCopyWriteError, job.issuePath, err)
		c.removeRootIfEmpty()
		return "", 0, 0, err
	}
	return path, h.Sum64(), n, nil
}

// isDuplicate reports whether the staged payload is byte-identical to a
// retained file with the same xxhash64 and size. A hash+size match still
// requires a byte compare, so a forced (or natural) collision stays a distinct
// output. If a retained representative cannot be re-read, the read error is
// recorded against the current source and the staged payload is kept as a
// distinct output rather than risk dropping data.
func (c *EnvCopier) isDuplicate(staged string, sum uint64, size int64, issuePath string) bool {
	for _, fp := range c.fingerprints[sum] {
		if fp.size != size {
			continue
		}
		equal, err := filesEqual(fp.path, staged)
		if err != nil {
			c.recordError(EnvCopyReadError, issuePath, err)
			return false
		}
		if equal {
			return true
		}
	}
	return false
}

// filesEqual streams two files through equal-sized chunks and reports byte
// equality. Read errors abort with err rather than guessing; a size mismatch
// (external mutation after the size precheck) is simply "not equal".
func filesEqual(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close() //nolint:errcheck // read-only probe
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close() //nolint:errcheck // read-only probe
	return readersEqual(fa, fb)
}

// readersEqual is filesEqual's testable core over raw readers. Real read
// errors (anything but EOF flavors) abort with err before any equality
// verdict, so a partial read can never masquerade as "different content".
func readersEqual(fa, fb io.Reader) (bool, error) {
	const chunk = 32 << 10
	bufA := make([]byte, chunk)
	bufB := make([]byte, chunk)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		for _, e := range []error{errA, errB} {
			if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
				return false, e
			}
		}
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if errA != nil || errB != nil {
			return true, nil // both streams ended; final chunks matched
		}
	}
}

// removeRootIfEmpty deletes c.root when it exists and contains no entries so a
// failed first write does not leave an empty sfl_*_secrets directory behind.
// Uses Remove (not RemoveAll) and only when empty — safe on Windows and Unix.
func (c *EnvCopier) removeRootIfEmpty() {
	if c == nil || c.root == "" {
		return
	}
	entries, err := os.ReadDir(c.root)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(c.root)
}

// copyFile copies src to dest, enforcing maxBytes against the opened source
// (not a pre-open Lstat). The size is re-checked via Stat after open, and the
// copy runs through io.LimitReader(maxBytes+1) so a source that grows between
// the stat and the read can never exceed the cap. If more than maxBytes bytes
// are copied, the partial destination is removed and errEnvCopyOverCap is
// returned (copyTree translates this into errTdataOverCap). Returns the
// actual number of bytes copied so callers account real data, not Lstat sizes.
func copyFile(src, dest string, maxBytes int64) (int64, error) {
	in, err := openReadNoFollow(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, os.ErrInvalid
	}
	if info.Size() > maxBytes {
		return 0, errEnvCopyOverCap
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, maxBytes+1))
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && n > maxBytes {
		_ = os.Remove(dest) // never leave a partial over-cap destination
		return n, errEnvCopyOverCap
	}
	return n, err
}

// uniquePath returns a non-existing path by suffixing _2, _3, … before the
// extension (and after the full name for dotfiles like ".env" → ".env_2").
// The bool result is retained for defensive API completeness; the unbounded
// search returns only successful candidates after the initial stat.
func uniquePath(path string) (string, bool) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path, true
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	dir := filepath.Dir(path)
	if base == "" {
		// Dotfile like ".env": filepath.Ext consumes the whole name as the
		// extension, leaving an empty stem. Suffix the full name instead so
		// ".env" -> ".env_2", not "_2.env".
		base = filepath.Base(path)
		ext = ""
	}
	for i := 2; ; i++ {
		candidate := filepath.Join(dir, base+"_"+itoa(i)+ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, true
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// copyTree recursively copies src into dest, returning the number of regular
// files copied. Non-regular nodes (symlinks, fifos, devices) are skipped so
// a planted fifo cannot hang the copier. used tracks bytes of regular files
// copied; exceeding TdataCopyMaxBytes returns errTdataOverCap. Callers wipe
// dest on any error.
func copyTree(src, dest string, used *int64) (int, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, nil
	}
	if info.IsDir() {
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return 0, err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return 0, err
		}
		var n int
		// Review H-18: distinct Unix filenames can normalize onto one
		// destination element (e.g. `ab` and `a\b` both sanitize to `ab`), so
		// the loose-tree copy must never silently overwrite. A same-directory
		// normalization collision fails the whole tree as a tdata collision;
		// the caller wipes dest and the source is flagged so -del keeps it.
		seenElems := make(map[string]string, len(entries))
		for _, e := range entries {
			s := filepath.Join(src, e.Name())
			dElem := sanitizePathElem(e.Name())
			if prev, taken := seenElems[dElem]; taken {
				return n, fmt.Errorf("%w: %q and %q in %s both normalize to %q", errTdataPathCollision, prev, e.Name(), src, dElem)
			}
			seenElems[dElem] = e.Name()
			d := filepath.Join(dest, dElem)
			m, err := copyTree(s, d, used)
			n += m
			if err != nil {
				return n, err
			}
		}
		return n, nil
	}
	if !info.Mode().IsRegular() {
		return 0, nil
	}
	if used != nil && *used+info.Size() > TdataCopyMaxBytes {
		return 0, errTdataOverCap
	}
	remaining := TdataCopyMaxBytes
	if used != nil {
		remaining -= *used
	}
	n, err := copyFile(src, dest, remaining)
	if err != nil {
		// copyFile reports the generic over-cap sentinel; the tree cap is the
		// tdata one. (Triggers when a source grew past the remaining budget
		// after the Lstat pre-check above.)
		if errors.Is(err, errEnvCopyOverCap) {
			return 0, errTdataOverCap
		}
		return 0, err
	}
	if used != nil {
		*used += n // actual copied bytes, not the earlier Lstat size
	}
	return 1, nil
}

func (c *EnvCopier) recordError(kind EnvCopyErrorKind, path string, err error) {
	c.recordErrorWithNotify(kind, path, err, true)
}

func (c *EnvCopier) recordErrorSilent(kind EnvCopyErrorKind, path string, err error) {
	c.recordErrorWithNotify(kind, path, err, false)
}

func (c *EnvCopier) recordErrorWithNotify(kind EnvCopyErrorKind, path string, err error, notify bool) {
	c.mu.Lock()
	c.stats.WriteErrors++
	c.stats.Issues = append(c.stats.Issues, EnvCopyIssue{Path: path, Kind: kind, Err: err})
	switch kind {
	case EnvCopyOpenError:
		c.stats.OpenErrors++
	case EnvCopyReadError:
		c.stats.ReadErrors++
	case EnvCopyWriteError:
		c.stats.WriteFailures++
	case EnvCopyCollisionError:
		c.stats.CollisionErrors++
	case EnvCopyTdataError:
		c.stats.TdataErrors++
	}
	fn := c.onError
	c.mu.Unlock()
	if notify && fn != nil {
		c.cb.run("error-handler", func() { fn(EnvCopyIssue{Path: path, Kind: kind, Err: err}) })
	}
}

func (c *EnvCopier) recordSkippedOverCap(path string) {
	issue := EnvCopyIssue{Path: path, Kind: EnvCopyOverCap, Err: errEnvCopyOverCap}
	c.mu.Lock()
	c.stats.SkippedOverCap++
	c.stats.Issues = append(c.stats.Issues, issue)
	fn := c.onError
	c.mu.Unlock()
	if fn != nil {
		c.cb.run("error-handler", func() { fn(issue) })
	}
}

// CallbackError returns the first run-level error recorded on this copier —
// an integration callback panic (see callbackGuard) or a lifecycle error such
// as an enqueue racing shutdown/cancellation — or nil. These are RUN errors,
// never copy issues: Engine.Run and cmd callers propagate them so a failed
// copier can never leave the run looking successful (which would allow source
// deletion).
func (c *EnvCopier) CallbackError() error {
	if c == nil {
		return nil
	}
	if err := c.cb.callbackErr(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runErr
}

func (c *EnvCopier) bumpSkippedTdataOverCap() {
	c.mu.Lock()
	c.stats.DirsSkippedOverCap++
	c.mu.Unlock()
}

// Close cancels the copier, drains every accepted job, waits for the worker,
// and returns final stats. Idempotent; safe without a prior Start (the worker
// lazy-starts so accepted jobs are still drained).
func (c *EnvCopier) Close() EnvCopyStats {
	if c == nil {
		return EnvCopyStats{}
	}
	c.ensureStarted()
	c.closeOnce.Do(func() {
		// Cancel first: blocked enqueues wake through ctx.Done, and the
		// closed flag under sendMu means no send can hit the closed queue.
		c.cancel()
		c.sendMu.Lock()
		c.closed = true
		close(c.queue)
		c.sendMu.Unlock()
	})
	c.wg.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}
