package sflog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode"
	zipenc "github.com/yeka/zip"
)

// maxArchiveDepth caps how deep we recurse into archives-within-archives. The
// outer archive is depth 0; nested members may go up to this many levels. It
// guards against zip bombs and pathological nesting.
const maxArchiveDepth = 3

// errNestTooDeep is recorded (never fatal) when a nested archive exceeds
// maxArchiveDepth.
var errNestTooDeep = errors.New("archive nesting too deep")

// errNotAnArchive is returned when a member's extension claims an archive but
// its leading bytes don't match the format's signature. Stealer logs routinely
// contain decoy/browser files named *.7z or *.zip that aren't archives; this
// lets the readers skip them as a parse/skip issue instead of burning a full
// password sweep and mislabeling them "password not found".
var errNotAnArchive = errors.New("not a recognized archive")

func isArchiveFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".zip", ".rar", ".7z":
		return true
	default:
		return false
	}
}

// archiveSignatureOK reports whether the file's leading bytes match the magic
// for ext. It only vets the offset-0 signature formats (.7z, .zip) the bodgit
// and yeka readers require there anyway; .rar (whose decoder scans for its
// marker and tolerates SFX stubs) and any other extension return ok=true so the
// normal reader still runs. A read error is returned so the caller can fall
// back rather than misclassify an I/O problem as "not an archive".
//
// Encrypted zips and 7z keep these signatures in the clear (only entry
// data/metadata is encrypted), so genuinely password-protected archives still
// pass.
func archiveSignatureOK(path, ext string) (bool, error) {
	switch strings.ToLower(ext) {
	case ".7z", ".zip":
	default:
		return true, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var hdr [6]byte
	n, err := io.ReadFull(f, hdr[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	head := hdr[:n]
	switch strings.ToLower(ext) {
	case ".7z":
		return len(head) >= 6 && string(head[:6]) == "\x37\x7A\xBC\xAF\x27\x1C", nil
	case ".zip":
		// Local file header, empty archive (EOCD), or spanned/data-descriptor.
		if len(head) < 4 || head[0] != 'P' || head[1] != 'K' {
			return false, nil
		}
		switch {
		case head[2] == 0x03 && head[3] == 0x04,
			head[2] == 0x05 && head[3] == 0x06,
			head[2] == 0x07 && head[3] == 0x08:
			return true, nil
		default:
			return false, nil
		}
	}
	return true, nil
}

// archiveScan reports what an (possibly recursive) archive read produced so the
// summary stays honest about nested members.
type archiveScan struct {
	files          int // credential files parsed, including nested
	nestedArchives int // archives found *inside* this one (recursively)
}

func (a *archiveScan) add(o archiveScan) {
	a.files += o.files
	a.nestedArchives += o.nestedArchives
}

// extractCtx carries everything the recursive archive readers need so they
// don't grow unwieldy parameter lists. It is copied (not shared) per recursion
// so depth/display are local to each level.
type extractCtx struct {
	passwords []string
	tempDir   string // spill location for nested members, "" = os.TempDir()
	depth     int
	display   string // logical provenance prefix, e.g. "a.zip!b.zip"
	emit      func(Credential)
	onIssue   func(path string, kind IssueKind, err error)
	p         *Progress
	// setStage (may be nil) publishes this worker's current archive stage to
	// the live TUI. Copied across recursion so nested members report too.
	setStage func(WorkerStage)
	// setItem (may be nil) publishes the provenance of the archive this worker
	// is currently inside (e.g. "outer.rar!inner.7z"). recurseNested re-points
	// it on descent and restores the parent on return so the live line names
	// the nested archive being worked on, not just the top-level item.
	setItem func(string)
	// sem (may be nil) is the engine-wide member-parallelism budget, sized to
	// the worker count and shared by every top-level archive. readZipFiles
	// drains it to run a big zip's members concurrently; nil disables member
	// parallelism (hermetic callers, or when the engine didn't wire one).
	sem chan struct{}
	// debug (may be nil) logs provenance-safe diagnostics (paths, counts,
	// elapsed) to the run's -debug log. Copied across recursion. Never carries
	// raw passwords or credential values.
	debug func(format string, args ...any)
	// hb (may be nil) throttles the "still extracting" heartbeat so a long
	// decode shows movement without flooding the log. Shared across recursion
	// (one per top-level archive tree).
	hb *debugThrottle
}

// stage publishes s to the worker slot if a stage sink is wired (no-op for
// direct/hermetic callers that don't drive the TUI).
func (ec extractCtx) stage(s WorkerStage) {
	if ec.setStage != nil {
		ec.setStage(s)
	}
}

// item publishes the provenance label of the archive this level is working on
// to the worker slot if an item sink is wired (no-op otherwise).
func (ec extractCtx) item(label string) {
	if ec.setItem != nil {
		ec.setItem(label)
	}
}

// minParallelZipMembers is the member count below which a zip is read
// sequentially: tiny archives aren't worth the goroutine/merge overhead and stay
// trivially deterministic.
const minParallelZipMembers = 8

// parallelMembers reports whether this level's zip members may be read through
// the shared pool: only at the top level (depth 0, so a member task never
// re-acquires a slot -> no hold-and-wait deadlock), only when a budget is wired,
// and only for archives with enough members to be worth it.
func (ec extractCtx) parallelMembers(n int) bool {
	return ec.sem != nil && ec.depth == 0 && n >= minParallelZipMembers
}

// debugf logs a provenance-safe diagnostic line when -debug is on (no-op
// otherwise).
func (ec extractCtx) debugf(format string, args ...any) {
	if ec.debug != nil {
		ec.debug(format, args...)
	}
}

// heartbeat emits a throttled "still extracting" line so a long archive shows
// progress between its open and completion lines. members is the count of
// members visited so far in this (possibly nested) archive.
func (ec extractCtx) heartbeat(members int) {
	if ec.debug == nil || !ec.hb.ready() {
		return
	}
	ec.debug("archive %s: still extracting (%d member(s), %s elapsed)",
		ec.display, members, time.Since(ec.hb.start).Round(time.Second))
}

// debugThrottle rate-limits heartbeat lines to at most one per interval and
// tracks the archive's start time for an "elapsed" readout. The zero value is
// unusable; build with newDebugThrottle.
type debugThrottle struct {
	mu       sync.Mutex
	interval time.Duration
	start    time.Time
	last     time.Time
}

func newDebugThrottle(interval time.Duration) *debugThrottle {
	now := time.Now()
	return &debugThrottle{interval: interval, start: now, last: now}
}

// ready reports whether interval has elapsed since the last emit, advancing the
// clock when it has. nil-safe (returns false) so callers needn't branch.
func (t *debugThrottle) ready() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if now.Sub(t.last) < t.interval {
		return false
	}
	t.last = now
	return true
}

// pendingIssue lets the streaming (rar/7z) readers buffer nested issues and
// only commit them once a password pass succeeds.
type pendingIssue struct {
	path string
	kind IssueKind
	err  error
}

// memberOutcome is one zip member's result, collected by a pool task into its
// own slot so the merge back into the shared emit/onIssue/scan sinks happens on
// a single goroutine, in member order (keeping output deterministic).
type memberOutcome struct {
	creds  []Credential
	issues []pendingIssue
	scan   archiveScan
	ctxErr error // set only on context cancellation
}

// boundedForEach runs fn(0..n-1) concurrently, capped at cap(sem) in flight by
// acquiring a slot before spawning (so a million-member archive never spawns a
// million goroutines). It stops dispatching once ctx is cancelled; in-flight
// tasks run to completion. sem is the engine-wide budget, shared across archives
// so total concurrency stays bounded by the worker count.
func boundedForEach(ctx context.Context, sem chan struct{}, n int, fn func(i int)) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// readArchiveCredentials extracts credentials from an archive at diskPath,
// recursing into nested archive members. weight drives smooth progress; ec.emit
// receives one credential at a time. It returns the files/nested-archive counts.
func readArchiveCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	// One heartbeat throttle per top-level archive tree: created at the outer
	// archive (hb nil) and inherited by nested members via the ec copy, so the
	// "still extracting" line is rate-limited across the whole recursion.
	if ec.hb == nil && ec.debug != nil {
		ec.hb = newDebugThrottle(5 * time.Second)
	}
	ext := strings.ToLower(filepath.Ext(diskPath))
	ec.debugf("archive %s: opening (%s, weight=%dB, passwords=%d, depth=%d)",
		ec.display, ext, weight, len(ec.passwords), ec.depth)
	// Vet the signature before any password sweep: a member named *.7z/*.zip
	// whose bytes don't match is a decoy, not a locked archive. Skip it cleanly
	// (errNotAnArchive -> parse/skip issue) instead of trying every password and
	// reporting "password not found". A read error falls through to the reader.
	if ok, sniffErr := archiveSignatureOK(diskPath, ext); sniffErr == nil && !ok {
		ec.debugf("archive %s: signature mismatch for %s, skipping (not a real archive)", ec.display, ext)
		return archiveScan{}, errNotAnArchive
	}
	switch ext {
	case ".zip":
		return readZipCredentials(ctx, diskPath, ec, weight)
	case ".rar":
		return readRarCredentials(ctx, diskPath, ec, weight)
	case ".7z":
		return readSevenZipCredentials(ctx, diskPath, ec, weight)
	default:
		return archiveScan{}, nil
	}
}

// scaleFor maps the uncompressed bytes we will read onto the archive's on-disk
// weight so within-archive progress sums to exactly weight.
func scaleFor(weight, uncompressed int64) float64 {
	if uncompressed <= 0 {
		return 1
	}
	return float64(weight) / float64(uncompressed)
}

// spillToTemp streams an archive member out to a temp file (preserving its
// extension so the recursive reader dispatches correctly) so a nested archive
// can be reopened by the path-based readers. cr (may be nil) credits the bytes
// read while spilling.
func spillToTemp(tempDir, name string, rc io.Reader, cr *creditor) (string, error) {
	tf, err := os.CreateTemp(tempDir, "sfl-nested-*"+filepath.Ext(name))
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(tf, countingReader{r: rc, c: cr})
	closeErr := tf.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tf.Name())
		return "", firstErr(copyErr, closeErr)
	}
	return tf.Name(), nil
}

// recurseNested spills a nested archive member to disk and re-runs the reader on
// it one level deeper. All extraction failures are isolated (recorded via
// ec.onIssue) so a bad nested archive never aborts the parent; only ctx
// cancellation propagates. spillCr (may be nil for rar, whose bytes are already
// counted by the outer file reader) credits the spill copy.
func recurseNested(ctx context.Context, ec extractCtx, open func() (io.ReadCloser, error), name string, spillCr *creditor) (archiveScan, error) {
	display := ec.display + "!" + name
	if ec.depth+1 > maxArchiveDepth {
		ec.onIssue(display, IssueParseError, fmt.Errorf("%w (limit %d)", errNestTooDeep, maxArchiveDepth))
		return archiveScan{}, nil
	}

	rc, err := open()
	if err != nil {
		ec.onIssue(display, IssueOpenError, err)
		return archiveScan{}, nil
	}
	tmp, spillErr := spillToTemp(ec.tempDir, name, rc, spillCr)
	_ = rc.Close()
	if spillErr != nil {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.onIssue(display, IssueParseError, spillErr)
		return archiveScan{}, nil
	}
	defer os.Remove(tmp)

	child := ec
	child.depth++
	child.display = display
	// Re-point the live worker line at the nested archive while it is worked,
	// then restore this level's label (and its extracting stage) once it
	// returns so the line never reads as the parent "testing password".
	ec.item(display)
	// Nested progress rides on the spill credit above; weight 0 keeps the
	// nested reader's own creditor from double-counting.
	scan, err := readArchiveCredentials(ctx, tmp, child, 0)
	ec.item(ec.display)
	ec.stage(StageExtracting)
	if err != nil {
		if ctx.Err() != nil {
			return scan, err
		}
		kind := IssueParseError
		if errors.Is(err, errPasswordNotFound) {
			kind = IssuePasswordNotFound
		}
		ec.onIssue(display, kind, err)
		return scan, nil
	}
	return scan, nil
}

// readZipCredentials opens a single-file zip by path and hands its members to
// readZipFiles. Split-zip sets reach readZipFiles via readSplitArchive instead,
// using a concatenated ReaderAt, so the member-handling logic lives in one place.
func readZipCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	zr, err := zipenc.OpenReader(diskPath)
	if err != nil {
		return archiveScan{}, err
	}
	defer zr.Close()
	return readZipFiles(ctx, zr.File, ec, weight)
}

// readZipFiles classifies a zip's members (credential files vs nested archives),
// resolves a single password against the smallest encrypted probe member, then
// reads/recurses each. It is fed either a path-opened zip (readZipCredentials)
// or a split set's concatenated reader (readSplitArchive).
func readZipFiles(ctx context.Context, files []*zipenc.File, ec extractCtx, weight int64) (archiveScan, error) {
	var credFiles, nestedFiles []*zipenc.File
	var probe *zipenc.File
	var uncompressed int64
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		switch {
		case isArchiveFile(f.Name):
			nestedFiles = append(nestedFiles, f)
		case isPasswordFile(f.Name):
			credFiles = append(credFiles, f)
		default:
			continue
		}
		uncompressed += int64(f.UncompressedSize64)
		// Probe with the *smallest* encrypted member so StageTestingPassword
		// stays a short blip even on a large (split) set, instead of decrypting
		// the first (possibly huge) member once per candidate password.
		if f.IsEncrypted() && (probe == nil || f.UncompressedSize64 < probe.UncompressedSize64) {
			probe = f
		}
	}
	if len(credFiles) == 0 && len(nestedFiles) == 0 {
		return archiveScan{}, nil
	}

	// Resolve a single working password against the smallest encrypted member,
	// then reuse it for all members. yeka/zip handles WinZip AES and legacy
	// ZipCrypto.
	pw := ""
	if probe != nil {
		ec.stage(StageTestingPassword)
		ec.debugf("archive %s: resolving password against probe member %s (%dB uncompressed, %d candidate(s))",
			ec.display, filepath.Base(probe.Name), probe.UncompressedSize64, len(ec.passwords))
		resolved, ok := resolveZipPassword(probe, ec.passwords)
		if !ok {
			return archiveScan{}, errPasswordNotFound
		}
		pw = resolved
	}
	ec.stage(StageExtracting)

	cr := newCreditor(ec.p, weight, scaleFor(weight, uncompressed))
	defer cr.finish()

	// Big top-level zips read their members through the shared pool; everything
	// else (small archives, nested members below depth 0) stays on the
	// sequential path so behavior and output order are unchanged.
	if ec.parallelMembers(len(credFiles) + len(nestedFiles)) {
		return readZipMembersParallel(ctx, credFiles, nestedFiles, ec, pw, cr)
	}

	var scan archiveScan
	members := 0
	for _, f := range credFiles {
		if ctx.Err() != nil {
			return scan, ctx.Err()
		}
		members++
		ec.heartbeat(members)
		o := readZipCredMember(ctx, f, ec, pw, cr)
		for _, is := range o.issues {
			ec.onIssue(is.path, is.kind, is.err)
		}
		scan.add(o.scan)
		for _, c := range o.creds {
			ec.emit(c)
		}
	}
	for _, f := range nestedFiles {
		if ctx.Err() != nil {
			return scan, ctx.Err()
		}
		members++
		ec.heartbeat(members)
		member := f
		open := func() (io.ReadCloser, error) {
			if member.IsEncrypted() {
				member.SetPassword(pw)
			}
			return member.Open()
		}
		ns, err := recurseNested(ctx, ec, open, member.Name, cr)
		if err != nil {
			return scan, err // ctx only
		}
		ns.nestedArchives++
		scan.add(ns)
	}
	return scan, nil
}

// readZipMembersParallel reads a top-level zip's credential members and nested
// archives through the shared member pool. Each task collects into its own
// outcome slot (lock-free), and results are merged in member order on return so
// output is deterministic regardless of completion order. The credit (cr) is
// the only shared sink touched concurrently and is atomic. Per-task stage/item
// sinks are dropped so concurrent tasks never thrash the single worker slot; the
// archive-level "extracting" stage set by the caller stands.
func readZipMembersParallel(ctx context.Context, credFiles, nestedFiles []*zipenc.File, ec extractCtx, pw string, cr *creditor) (archiveScan, error) {
	out := make([]memberOutcome, len(credFiles)+len(nestedFiles))
	var members atomic.Int64
	boundedForEach(ctx, ec.sem, len(out), func(i int) {
		if ctx.Err() != nil {
			out[i].ctxErr = ctx.Err()
			return
		}
		ec.heartbeat(int(members.Add(1)))
		if i < len(credFiles) {
			out[i] = readZipCredMember(ctx, credFiles[i], ec, pw, cr)
			return
		}
		out[i] = readZipNestedMember(ctx, nestedFiles[i-len(credFiles)], ec, pw, cr)
	})
	var scan archiveScan
	for i := range out {
		if out[i].ctxErr != nil {
			return scan, out[i].ctxErr
		}
		for _, is := range out[i].issues {
			ec.onIssue(is.path, is.kind, is.err)
		}
		scan.add(out[i].scan)
		for _, c := range out[i].creds {
			ec.emit(c)
		}
	}
	return scan, nil
}

// readZipCredMember opens, decrypts and parses one credential member, returning
// a self-contained outcome (no shared sinks touched except the atomic cr). A bad
// member becomes an isolated issue and never discards the rest of the archive.
func readZipCredMember(ctx context.Context, f *zipenc.File, ec extractCtx, pw string, cr *creditor) memberOutcome {
	var o memberOutcome
	name := ec.display + "!" + f.Name
	if f.IsEncrypted() {
		f.SetPassword(pw)
	}
	rc, err := f.Open()
	if err != nil {
		o.issues = append(o.issues, pendingIssue{name, IssueOpenError, err})
		return o
	}
	creds, parseErr := ParseCredentials(countingReader{r: rc, c: cr}, name)
	closeErr := rc.Close()
	if parseErr != nil || closeErr != nil {
		o.issues = append(o.issues, pendingIssue{name, IssueParseError, firstErr(parseErr, closeErr)})
		return o
	}
	o.scan.files++
	o.creds = creds
	return o
}

// readZipNestedMember recurses into one nested archive member, collecting its
// creds/issues into a task-local outcome. The live stage/item sinks are dropped
// so concurrent nested tasks don't fight over the single worker slot; the
// recursion runs sequentially (depth > 0, so parallelMembers is false).
func readZipNestedMember(ctx context.Context, f *zipenc.File, ec extractCtx, pw string, cr *creditor) memberOutcome {
	var o memberOutcome
	taskEc := ec
	taskEc.emit = func(c Credential) { o.creds = append(o.creds, c) }
	taskEc.onIssue = func(p string, k IssueKind, e error) { o.issues = append(o.issues, pendingIssue{p, k, e}) }
	taskEc.setStage = nil
	taskEc.setItem = nil
	member := f
	open := func() (io.ReadCloser, error) {
		if member.IsEncrypted() {
			member.SetPassword(pw)
		}
		return member.Open()
	}
	ns, err := recurseNested(ctx, taskEc, open, member.Name, cr)
	if err != nil {
		o.ctxErr = err // ctx only
		return o
	}
	ns.nestedArchives++
	o.scan = ns
	return o
}

// resolveZipPassword finds the first candidate that fully decrypts the
// (encrypted) probe member. Reading one member validates the password; it is
// then reused for every member of the archive.
func resolveZipPassword(m *zipenc.File, passwords []string) (string, bool) {
	for _, pw := range passwords {
		m.SetPassword(pw)
		rc, err := m.Open()
		if err != nil {
			continue
		}
		_, copyErr := io.Copy(io.Discard, rc)
		closeErr := rc.Close()
		if copyErr == nil && closeErr == nil {
			return pw, true
		}
	}
	return "", false
}

func readRarCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	// One creditor for the whole item: it clamps to weight, so retries across
	// passwords never over-credit progress. rar is streaming, so we buffer
	// credentials (and nested issues) and only commit after a clean EOF (guards
	// against a wrong password yielding partial garbage).
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	var lastErr error
	for i, pw := range ec.passwords {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.stage(StageTestingPassword)
		ec.debugf("archive %s: trying password %d/%d", ec.display, i+1, len(ec.passwords))
		attemptStart := time.Now()
		f, err := os.Open(diskPath)
		if err != nil {
			return archiveScan{}, err
		}
		unreg := registerAbort(ctx, f)
		rr, err := rardecode.NewReader(countingReader{r: f, c: cr}, pw)
		if err != nil {
			unreg()
			_ = f.Close()
			lastErr = err
			ec.debugf("archive %s: password %d/%d rejected after %s: %v",
				ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond), err)
			continue
		}

		var bufCreds []Credential
		var bufIssues []pendingIssue
		bufEc := ec
		bufEc.emit = func(c Credential) { bufCreds = append(bufCreds, c) }
		bufEc.onIssue = func(path string, kind IssueKind, err error) {
			bufIssues = append(bufIssues, pendingIssue{path, kind, err})
		}
		scan, streamErr := readRarStream(ctx, bufEc, rr)
		unreg()
		_ = f.Close()
		if streamErr == nil {
			for _, c := range bufCreds {
				ec.emit(c)
			}
			for _, is := range bufIssues {
				ec.onIssue(is.path, is.kind, is.err)
			}
			return scan, nil
		}
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		lastErr = streamErr
		ec.debugf("archive %s: password %d/%d failed after %s: %v",
			ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond), streamErr)
	}
	if lastErr == nil {
		lastErr = errPasswordNotFound
	}
	return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

func readRarStream(ctx context.Context, ec extractCtx, rr *rardecode.Reader) (archiveScan, error) {
	ec.stage(StageExtracting)
	var scan archiveScan
	members := 0
	validated := false
	for {
		if ctx.Err() != nil {
			return scan, ctx.Err()
		}
		h, err := rr.Next()
		if errors.Is(err, io.EOF) {
			return scan, nil
		}
		if err != nil {
			return scan, err
		}
		if h.IsDir {
			continue
		}
		members++
		ec.heartbeat(members)
		// Force the first member's body through the decoder so a wrong password
		// fails on its first CRC check, instead of (for solid archives)
		// decompressing every member up to the first credential file. Members
		// the switch already reads in full need no separate drain.
		if !validated {
			validated = true
			if !isArchiveFile(h.Name) && !isPasswordFile(h.Name) {
				if _, derr := io.Copy(io.Discard, rr); derr != nil {
					return scan, derr
				}
				continue
			}
		}
		switch {
		case isArchiveFile(h.Name):
			// The current entry's body streams from rr; spill it now (before the
			// next Next()). Bytes are already counted by the outer file reader,
			// so the spill creditor is nil.
			open := func() (io.ReadCloser, error) { return io.NopCloser(rr), nil }
			ns, rerr := recurseNested(ctx, ec, open, h.Name, nil)
			if rerr != nil {
				return scan, rerr // ctx only
			}
			ns.nestedArchives++
			scan.add(ns)
		case isPasswordFile(h.Name):
			creds, perr := ParseCredentials(rr, ec.display+"!"+h.Name)
			if perr != nil {
				return scan, perr
			}
			scan.files++
			for _, c := range creds {
				ec.emit(c)
			}
		}
	}
}

// readRarVolumes reads a new-style multi-volume RAR set (name.part1.rar,
// .part2.rar, ...). Unlike the single-file path it uses rardecode.OpenReader,
// which follows the on-disk volume sequence by name, so members that span
// volume boundaries decode correctly. Progress is credited per completed
// volume (coarser than the byte-accurate single-file path, but this is the rare
// case); finish() tops up any remainder. Like the single-file path it buffers
// credentials and commits only after a clean EOF so a wrong password yields no
// partial output.
func readRarVolumes(ctx context.Context, volumes []string, ec extractCtx, weight int64) (archiveScan, error) {
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	first := volumes[0]
	var lastErr error
	for i, pw := range ec.passwords {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.stage(StageTestingPassword)
		ec.debugf("archive %s: trying password %d/%d (multi-volume, %d parts)",
			ec.display, i+1, len(ec.passwords), len(volumes))
		attemptStart := time.Now()
		rc, err := rardecode.OpenReader(first, pw)
		if err != nil {
			lastErr = err
			ec.debugf("archive %s: password %d/%d rejected after %s: %v",
				ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond), err)
			continue
		}

		var bufCreds []Credential
		var bufIssues []pendingIssue
		bufEc := ec
		bufEc.emit = func(c Credential) { bufCreds = append(bufCreds, c) }
		bufEc.onIssue = func(path string, kind IssueKind, err error) {
			bufIssues = append(bufIssues, pendingIssue{path, kind, err})
		}
		scan, streamErr := readRarVolumeStream(ctx, bufEc, rc, cr)
		_ = rc.Close()
		if streamErr == nil {
			for _, c := range bufCreds {
				ec.emit(c)
			}
			for _, is := range bufIssues {
				ec.onIssue(is.path, is.kind, is.err)
			}
			return scan, nil
		}
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		lastErr = streamErr
		ec.debugf("archive %s: password %d/%d failed after %s: %v",
			ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond), streamErr)
	}
	if lastErr == nil {
		lastErr = errPasswordNotFound
	}
	return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

// readRarVolumeStream mirrors readRarStream but credits progress per finished
// volume (the OpenReader path can't tee individual file reads) and applies the
// same first-member validation to reject wrong passwords quickly.
func readRarVolumeStream(ctx context.Context, ec extractCtx, rc *rardecode.ReadCloser, cr *creditor) (archiveScan, error) {
	ec.stage(StageExtracting)
	var scan archiveScan
	members := 0
	validated := false
	credited := 0
	// creditUpTo credits the on-disk size of every volume fully consumed so far
	// (all but the currently open one). rc.Volumes() grows as decoding crosses
	// volume boundaries and always includes the open volume last.
	creditUpTo := func(n int) {
		vols := rc.Volumes()
		if n > len(vols) {
			n = len(vols)
		}
		for ; credited < n; credited++ {
			if fi, err := os.Stat(vols[credited]); err == nil {
				cr.add(fi.Size())
			}
		}
	}
	for {
		if ctx.Err() != nil {
			return scan, ctx.Err()
		}
		h, err := rc.Next()
		creditUpTo(len(rc.Volumes()) - 1) // last entry is the still-open volume
		if errors.Is(err, io.EOF) {
			return scan, nil
		}
		if err != nil {
			return scan, err
		}
		if h.IsDir {
			continue
		}
		members++
		ec.heartbeat(members)
		if !validated {
			validated = true
			if !isArchiveFile(h.Name) && !isPasswordFile(h.Name) {
				if _, derr := io.Copy(io.Discard, rc); derr != nil {
					return scan, derr
				}
				continue
			}
		}
		switch {
		case isArchiveFile(h.Name):
			open := func() (io.ReadCloser, error) { return io.NopCloser(rc), nil }
			ns, rerr := recurseNested(ctx, ec, open, h.Name, nil)
			if rerr != nil {
				return scan, rerr
			}
			ns.nestedArchives++
			scan.add(ns)
		case isPasswordFile(h.Name):
			creds, perr := ParseCredentials(rc, ec.display+"!"+h.Name)
			if perr != nil {
				return scan, perr
			}
			scan.files++
			for _, c := range creds {
				ec.emit(c)
			}
		}
	}
}

// readSevenZipCredentials reads a single-file 7z by path. The split-set caller
// uses readSevenZip directly with a ReaderAt-backed factory.
func readSevenZipCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	return readSevenZip(ctx, ec, weight, func(pw string) (*sevenzip.Reader, func() error, error) {
		rc, err := sevenzip.OpenReaderWithPassword(diskPath, pw)
		if err != nil {
			return nil, nil, err
		}
		return &rc.Reader, rc.Close, nil
	})
}

// readSevenZip drives the password sweep over a 7z archive independent of where
// the bytes come from. open(pw) returns a reader for one password attempt plus a
// closer for that attempt; the path caller wraps OpenReaderWithPassword, the
// split caller wraps NewReaderWithPassword over the concatenated parts. Each
// candidate is tried in a single pass; credentials/issues are buffered and only
// committed after every member decrypts and parses cleanly, so a wrong password
// (which fails mid-read) yields no partial/garbage output.
func readSevenZip(ctx context.Context, ec extractCtx, weight int64, open func(pw string) (*sevenzip.Reader, func() error, error)) (archiveScan, error) {
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	var lastErr error
	emptyArchive := false
	for i, pw := range ec.passwords {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.stage(StageTestingPassword)
		ec.debugf("archive %s: trying password %d/%d", ec.display, i+1, len(ec.passwords))
		attemptStart := time.Now()
		zr, closeReader, err := open(pw)
		if err != nil {
			lastErr = err // header-encrypted wrong password fails here
			ec.debugf("archive %s: password %d/%d rejected after %s: %v",
				ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond), err)
			continue
		}

		var bufCreds []Credential
		var bufIssues []pendingIssue
		bufEc := ec
		bufEc.emit = func(c Credential) { bufCreds = append(bufCreds, c) }
		bufEc.onIssue = func(path string, kind IssueKind, err error) {
			bufIssues = append(bufIssues, pendingIssue{path, kind, err})
		}
		scan, hadMembers, streamErr := readSevenZipMembers(ctx, bufEc, zr, cr)
		_ = closeReader()
		if streamErr == nil {
			if !hadMembers {
				emptyArchive = true
				break
			}
			for _, c := range bufCreds {
				ec.emit(c)
			}
			for _, is := range bufIssues {
				ec.onIssue(is.path, is.kind, is.err)
			}
			return scan, nil
		}
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		lastErr = streamErr
		ec.debugf("archive %s: password %d/%d failed after %s: %v",
			ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond), streamErr)
	}
	if emptyArchive {
		return archiveScan{}, nil
	}
	if lastErr == nil {
		lastErr = errPasswordNotFound
	}
	return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

func readSevenZipMembers(ctx context.Context, ec extractCtx, zr *sevenzip.Reader, cr *creditor) (archiveScan, bool, error) {
	ec.stage(StageExtracting)
	var scan archiveScan
	hadMembers := false
	members := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		isArch := isArchiveFile(f.Name)
		if !isArch && !isPasswordFile(f.Name) {
			continue
		}
		hadMembers = true
		members++
		ec.heartbeat(members)
		if ctx.Err() != nil {
			return scan, hadMembers, ctx.Err()
		}
		member := f
		if isArch {
			open := func() (io.ReadCloser, error) { return member.Open() }
			ns, rerr := recurseNested(ctx, ec, open, member.Name, cr)
			if rerr != nil {
				return scan, hadMembers, rerr // ctx only
			}
			ns.nestedArchives++
			scan.add(ns)
			continue
		}
		rc, err := member.Open()
		if err != nil {
			return scan, hadMembers, err
		}
		creds, parseErr := ParseCredentials(rc, ec.display+"!"+member.Name)
		closeErr := rc.Close()
		if parseErr != nil || closeErr != nil {
			return scan, hadMembers, firstErr(parseErr, closeErr)
		}
		scan.files++
		for _, c := range creds {
			ec.emit(c)
		}
	}
	return scan, hadMembers, nil
}

// readSplitArchive reads a raw byte-split set (".zip.NNN" / ".7z.NNN") as one
// logical archive. The ordered parts are presented as a single concatenated
// io.ReaderAt (no temp reassembly), then dispatched to the same zip/7z member
// logic the single-file path uses — so nested archives, the signature sniff,
// password probing and the live worker line all apply unchanged. The logical
// format is the parts' name with the ".NNN" suffix stripped.
func readSplitArchive(ctx context.Context, parts []string, ec extractCtx, weight int64) (archiveScan, error) {
	if ec.hb == nil && ec.debug != nil {
		ec.hb = newDebugThrottle(5 * time.Second)
	}
	ra, err := openMultiPartReaderAt(parts)
	if err != nil {
		return archiveScan{}, err
	}
	defer ra.Close()

	logical := strings.TrimSuffix(parts[0], filepath.Ext(parts[0])) // drop ".NNN"
	ext := strings.ToLower(filepath.Ext(logical))
	ec.debugf("archive %s: opening split set (%d parts, %s, weight=%dB)",
		ec.display, len(parts), ext, weight)
	switch ext {
	case ".zip":
		zr, err := zipenc.NewReader(ra, ra.Size())
		if err != nil {
			return archiveScan{}, err
		}
		return readZipFiles(ctx, zr.File, ec, weight)
	case ".7z":
		return readSevenZip(ctx, ec, weight, func(pw string) (*sevenzip.Reader, func() error, error) {
			// ra is closed by this function's defer, not per attempt.
			r, err := sevenzip.NewReaderWithPassword(ra, ra.Size(), pw)
			if err != nil {
				return nil, nil, err
			}
			return r, func() error { return nil }, nil
		})
	default:
		return archiveScan{}, errNotAnArchive
	}
}
