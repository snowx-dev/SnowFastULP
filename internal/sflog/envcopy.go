package sflog

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

const defaultEnvCopyMaxLen = 16 << 20 // 16 MiB

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

// safeRelPath sanitizes a relative path for writing under the secrets folder.
func safeRelPath(rel string) string {
	rel = filepath.Clean(filepath.FromSlash(strings.ReplaceAll(rel, "\\", "/")))
	parts := strings.Split(rel, string(filepath.Separator))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			continue
		}
		out = append(out, sanitizePathElem(p))
	}
	if len(out) == 0 {
		return "_"
	}
	return filepath.Join(out...)
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
	if info.Mode()&os.ModeSymlink != 0 {
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
	if err := os.MkdirAll(c.root, 0o755); err != nil {
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
	if info.Mode()&os.ModeSymlink != 0 {
		return os.ErrInvalid
	}
	in, err := os.Open(src)
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
