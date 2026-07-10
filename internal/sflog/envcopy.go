package sflog

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	defaultEnvCopyMaxLen    = 16 << 20 // 16 MiB
	maxPendingContextFiles  = 2048
	maxPendingContextBytes  = 64 << 20 // 64 MiB
)

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

// logContextBasenames are victim metadata files copied once per log when env
// files are found, to give context to the extracted secrets.
var logContextBasenames = map[string]bool{
	"information.txt": true, "userinformation.txt": true, "user information.txt": true,
	"info.txt": true, "system.txt": true, "system info.txt": true,
	"machineinfo.txt": true, "userinfo.txt": true, "pc_info.txt": true, "specs.txt": true,
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

// isLogContextFile reports whether a basename is victim metadata worth copying
// alongside env files for context.
func isLogContextFile(name string) bool {
	return logContextBasenames[strings.ToLower(filepath.Base(name))]
}

// memberRelDest maps archive provenance to a path inside the log slug folder.
// The slug already identifies the log/archive, so top-level members use only
// their in-archive path. Nested archives prefix the inner archive basename.
func memberRelDest(display, memberName string) string {
	member := safeRelPath(filepath.ToSlash(memberName))
	if !strings.Contains(display, "!") {
		return member
	}
	var parts []string
	for _, seg := range strings.Split(display, "!") {
		parts = append(parts, sanitizePathElem(filepath.Base(seg)))
	}
	if len(parts) > 0 {
		parts = parts[1:] // drop outer archive; slug already names the log unit
	}
	if len(parts) == 0 {
		return member
	}
	return safeRelPath(filepath.Join(append(parts, member)...))
}

// safeRelPath sanitizes a relative path for writing under a log folder.
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

func logSlug(logKey string) string {
	return sanitizePathElem(filepath.Base(logKey))
}

// EnvCopyStats holds final -env copy counters.
type EnvCopyStats struct {
	Copied, ContextCopied, SkippedOverCap, WriteErrors int
}

type envJob struct {
	logKey     string
	relDest    string
	data       []byte
	srcPath    string
	memberName string // in-archive path for archive env files
	isContext  bool
}

type envPending struct {
	relDest    string
	data       []byte
	memberName string // in-archive path for anchor lookup at flush
}

type deferredContextFlush struct {
	logKey           string
	envCopiedMembers []string
	pending          []envPending
}

type envIndexEntry struct {
	dest, source string
}

// envBucketKey identifies one victim folder under a log slug for index grouping.
func envBucketKey(slug, victim string) string {
	if victim == "" {
		return slug
	}
	return filepath.Join(slug, victim)
}

// envVictimPrefix maps an in-log member path to the victim subfolder name under
// the archive slug. Empty means the log unit is already one victim (loose dir).
func envVictimPrefix(memberPath string) string {
	p := normalizeMemberPath(memberPath)
	if !strings.Contains(p, "/") {
		return ""
	}
	parts := strings.Split(p, "/")
	if parts[0] == "Batch" && len(parts) >= 3 {
		return safeRelPath(filepath.Join(parts[0], parts[1], parts[2]))
	}
	if strings.Contains(parts[0], "Logs") && len(parts) >= 2 {
		return safeRelPath(filepath.Join(parts[0], parts[1]))
	}
	return sanitizePathElem(parts[0])
}

func memberPathForVictim(job envJob) string {
	if job.memberName != "" {
		return job.memberName
	}
	return job.relDest
}

func envDestBasename(job envJob) string {
	if job.isContext && strings.ToLower(filepath.Base(job.relDest)) == "information.txt" {
		return "information.txt"
	}
	return flatBasename(job.relDest)
}

func normalizeMemberPath(name string) string {
	return filepath.ToSlash(filepath.Clean(name))
}

func contextAnchorDir(memberName string) string {
	return filepath.Dir(normalizeMemberPath(memberName))
}

func contextAnchorsFromMembers(members []string) []string {
	seen := make(map[string]bool)
	var anchors []string
	for _, m := range members {
		a := contextAnchorDir(m)
		if seen[a] {
			continue
		}
		seen[a] = true
		anchors = append(anchors, a)
	}
	return anchors
}

// pathUnderAnchor reports whether path is at or under anchor within the archive.
func pathUnderAnchor(path, anchor string) bool {
	path = normalizeMemberPath(path)
	anchor = normalizeMemberPath(anchor)
	if anchor == "" || anchor == "." {
		return !strings.Contains(path, "/")
	}
	return path == anchor || strings.HasPrefix(path, anchor+"/")
}

// markedContextAnchors returns context anchor dirs that had at least one env file
// copied under them in the same archive.
func markedContextAnchors(envMembers, contextMembers []string) map[string]bool {
	anchors := contextAnchorsFromMembers(contextMembers)
	marked := make(map[string]bool)
	for _, envPath := range envMembers {
		envPath = normalizeMemberPath(envPath)
		best := ""
		for _, a := range anchors {
			if !pathUnderAnchor(envPath, a) {
				continue
			}
			if len(a) > len(best) {
				best = a
			}
		}
		if best != "" {
			marked[best] = true
		}
		parent := contextAnchorDir(envPath)
		if parent != "" {
			marked[parent] = true
		}
	}
	return marked
}

// contextCopyAllowed reports whether a buffered context member should be copied
// given the anchors marked by env files in the same archive.
func contextCopyAllowed(contextMember string, marked map[string]bool) bool {
	if len(marked) == 0 {
		return false
	}
	dir := contextAnchorDir(contextMember)
	for m := range marked {
		if m == "." || m == "" {
			if dir == "." {
				return true
			}
			continue
		}
		if dir == m || pathUnderAnchor(dir, m) {
			return true
		}
	}
	return false
}

// appendPendingContext buffers a context member when under per-archive caps.
func appendPendingContext(state *archiveEnvState, p envPending, copier *EnvCopier) bool {
	if state == nil {
		return false
	}
	if len(state.pending) >= maxPendingContextFiles {
		if copier != nil {
			copier.bumpSkippedOverCap()
		}
		return false
	}
	if state.pendingContextBytes+int64(len(p.data)) > maxPendingContextBytes {
		if copier != nil {
			copier.bumpSkippedOverCap()
		}
		return false
	}
	state.pending = append(state.pending, p)
	state.pendingContextBytes += int64(len(p.data))
	return true
}

// EnvCopier asynchronously copies env/key files to root/<logSlug>/.
type EnvCopier struct {
	root    string
	prog    *Progress
	maxLen  int64
	queue   chan envJob
	wg      sync.WaitGroup
	started atomic.Bool

	mu                 sync.Mutex
	stats              EnvCopyStats
	looseContextDone   map[string]bool
	looseEnvMembers    map[string][]string
	writtenArchiveEnv  map[string][]string
	deferred           []deferredContextFlush
	index              map[string][]envIndexEntry
}

// NewEnvCopier creates a copier writing under root (…/env/<stamp>/).
func NewEnvCopier(root string, prog *Progress, maxLen int64) *EnvCopier {
	if maxLen <= 0 {
		maxLen = defaultEnvCopyMaxLen
	}
	return &EnvCopier{
		root:              root,
		prog:              prog,
		maxLen:            maxLen,
		queue:             make(chan envJob, 256),
		looseContextDone:  make(map[string]bool),
		looseEnvMembers:   make(map[string][]string),
		writtenArchiveEnv: make(map[string][]string),
		index:             make(map[string][]envIndexEntry),
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

// Root returns the timestamped env output directory.
func (c *EnvCopier) Root() string {
	if c == nil {
		return ""
	}
	return c.root
}

// EnqueueBytes queues in-memory member bytes for async copy.
func (c *EnvCopier) EnqueueBytes(logKey, relDest string, data []byte, isContext bool) {
	if c == nil || len(data) == 0 {
		return
	}
	if int64(len(data)) > c.maxLen {
		c.bumpSkippedOverCap()
		return
	}
	c.queue <- envJob{logKey: logKey, relDest: relDest, data: data, isContext: isContext}
}

func (c *EnvCopier) enqueueEnvBytes(logKey, relDest string, data []byte, memberName string) {
	if c == nil || len(data) == 0 {
		return
	}
	if int64(len(data)) > c.maxLen {
		c.bumpSkippedOverCap()
		return
	}
	c.queue <- envJob{
		logKey: logKey, relDest: relDest, data: data,
		memberName: memberName, isContext: false,
	}
}

// EnqueueFile queues a loose on-disk file for async copy.
func (c *EnvCopier) EnqueueFile(logKey, srcPath string, isContext bool) {
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
	if !isContext && info.Size() > c.maxLen {
		c.bumpSkippedOverCap()
		return
	}
	rel, err := filepath.Rel(logKey, srcPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = filepath.Base(srcPath)
	}
	c.queue <- envJob{logKey: logKey, relDest: rel, srcPath: srcPath, isContext: isContext}
}

func (c *EnvCopier) recordLooseEnv(logKey, relDest string) {
	c.mu.Lock()
	rel := normalizeMemberPath(relDest)
	c.looseEnvMembers[logKey] = append(c.looseEnvMembers[logKey], rel)
	c.mu.Unlock()
}

func (c *EnvCopier) recordWrittenArchiveEnv(logKey, memberName string) {
	c.mu.Lock()
	c.writtenArchiveEnv[logKey] = append(c.writtenArchiveEnv[logKey], normalizeMemberPath(memberName))
	c.mu.Unlock()
}

func (c *EnvCopier) enqueueLooseContext(logKey string) {
	c.mu.Lock()
	if c.looseContextDone[logKey] {
		c.mu.Unlock()
		return
	}
	c.looseContextDone[logKey] = true
	envMembers := append([]string(nil), c.looseEnvMembers[logKey]...)
	c.mu.Unlock()
	if len(envMembers) == 0 {
		return
	}

	info, err := os.Stat(logKey)
	if err != nil || !info.IsDir() {
		return
	}

	var contextMembers, contextPaths []string
	_ = filepath.WalkDir(logKey, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if isArchiveFile(path) || !isLogContextFile(path) {
			return nil
		}
		rel, err := filepath.Rel(logKey, path)
		if err != nil {
			return nil
		}
		contextMembers = append(contextMembers, normalizeMemberPath(rel))
		contextPaths = append(contextPaths, path)
		return nil
	})
	if len(contextMembers) == 0 {
		return
	}

	marked := markedContextAnchors(envMembers, contextMembers)
	for i, member := range contextMembers {
		if !contextCopyAllowed(member, marked) {
			continue
		}
		c.writeJob(envJob{
			logKey: logKey, relDest: member, srcPath: contextPaths[i], isContext: true,
		})
	}
}

// CopyMember reads up to maxLen from r and enqueues when name is a candidate.
// Returns true only when bytes were queued for copy.
func (c *EnvCopier) CopyMember(ctx context.Context, logKey, display, memberName string, r io.Reader) bool {
	if c == nil || !isEnvCopyCandidate(memberName) {
		return false
	}
	max := c.maxLen
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	io.Copy(io.Discard, r)
	if len(data) == 0 {
		return false
	}
	if int64(len(data)) > max {
		c.bumpSkippedOverCap()
		return false
	}
	if err != nil && len(data) == 0 {
		return false
	}
	c.enqueueEnvBytes(logKey, memberRelDest(display, memberName), data, memberName)
	return true
}

// FlushArchiveContext defers buffered context members for gating after env writes complete.
func (c *EnvCopier) FlushArchiveContext(logKey string, envCopiedMembers []string, pending []envPending) {
	if c == nil || len(envCopiedMembers) == 0 || len(pending) == 0 {
		return
	}
	c.mu.Lock()
	c.deferred = append(c.deferred, deferredContextFlush{
		logKey: logKey, envCopiedMembers: envCopiedMembers, pending: pending,
	})
	c.mu.Unlock()
}

func (c *EnvCopier) processDeferredContext() {
	c.mu.Lock()
	deferred := c.deferred
	c.deferred = nil
	written := make(map[string][]string, len(c.writtenArchiveEnv))
	for k, v := range c.writtenArchiveEnv {
		written[k] = append([]string(nil), v...)
	}
	c.mu.Unlock()

	for _, batch := range deferred {
		w := written[batch.logKey]
		if len(w) == 0 {
			continue
		}
		contextMembers := make([]string, len(batch.pending))
		for i, p := range batch.pending {
			contextMembers[i] = p.memberName
		}
		marked := markedContextAnchors(w, contextMembers)
		for _, p := range batch.pending {
			if !contextCopyAllowed(p.memberName, marked) {
				continue
			}
			c.writeJob(envJob{
				logKey: batch.logKey, relDest: p.relDest, data: p.data, isContext: true,
			})
		}
	}
}

func readUpTo(ctx context.Context, r io.Reader, max int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	lr := io.LimitReader(r, max+1)
	return io.ReadAll(lr)
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
	slug := logSlug(job.logKey)
	victim := envVictimPrefix(memberPathForVictim(job))
	destDir := filepath.Join(c.root, slug)
	if victim != "" {
		destDir = filepath.Join(destDir, victim)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		c.bumpWriteError()
		return
	}
	dest := uniquePath(filepath.Join(destDir, envDestBasename(job)))
	var err error
	if job.srcPath != "" {
		err = copyFile(job.srcPath, dest)
	} else {
		err = os.WriteFile(dest, job.data, 0o600)
	}
	if err != nil {
		c.bumpWriteError()
		return
	}
	c.mu.Lock()
	if job.isContext {
		c.stats.ContextCopied++
	} else {
		c.stats.Copied++
	}
	c.index[envBucketKey(slug, victim)] = append(c.index[envBucketKey(slug, victim)], envIndexEntry{
		dest:   filepath.Base(dest),
		source: normalizeMemberPath(job.relDest),
	})
	c.mu.Unlock()
	if c.prog != nil {
		c.prog.addEnvCopied(1)
	}
	if !job.isContext {
		if job.memberName != "" {
			c.recordWrittenArchiveEnv(job.logKey, job.memberName)
		} else {
			c.recordLooseEnv(job.logKey, job.relDest)
		}
		c.enqueueLooseContext(job.logKey)
	}
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

func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(filepath.Base(path), ext)
	dir := filepath.Dir(path)
	for i := 2; i < 1000; i++ {
		candidate := filepath.Join(dir, base+"_"+itoa(i)+ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return path
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

func (c *EnvCopier) writeIndexFiles() {
	c.mu.Lock()
	indexes := make(map[string][]envIndexEntry, len(c.index))
	for slug, entries := range c.index {
		cp := append([]envIndexEntry(nil), entries...)
		indexes[slug] = cp
	}
	c.mu.Unlock()

	for bucket, entries := range indexes {
		if len(entries) == 0 {
			continue
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].source < entries[j].source
		})
		var b strings.Builder
		for _, e := range entries {
			b.WriteString(e.dest)
			b.WriteByte('\t')
			b.WriteString(e.source)
			b.WriteByte('\n')
		}
		destDir := filepath.Join(c.root, bucket)
		if err := os.WriteFile(filepath.Join(destDir, "index.txt"), []byte(b.String()), 0o644); err != nil {
			c.bumpWriteError()
		}
	}
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
	c.processDeferredContext()
	c.writeIndexFiles()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// readContextMember reads a small context file from a streaming archive member.
func readContextMember(ctx context.Context, r io.Reader, max int64) []byte {
	data, err := readUpTo(ctx, r, max)
	if err != nil || len(data) == 0 {
		io.Copy(io.Discard, r)
		return nil
	}
	// Drain remainder so the archive stream advances.
	io.Copy(io.Discard, r)
	return data
}
