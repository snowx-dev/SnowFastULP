package sflog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
var errNotAnArchive = ErrNotAnArchive

// errIncompleteVolumeSet marks a multi-volume RAR set whose continuation
// volumes run past the parts present on disk (a truncated download). The parts
// we do have decoded cleanly, so it is surfaced as a missing-volume skip note
// (not a failure) and the credentials read so far are committed.
var errIncompleteVolumeSet = errors.New("incomplete multi-volume set")

// isMissingVolume reports whether err is a "next volume file not found" error.
// rardecode returns it (as an *os.PathError) when a multi-volume set is
// truncated: a continuation part is flagged "continues in next volume" but the
// next part is absent. It is structural, so retrying other passwords on it only
// re-streams the whole archive for nothing.
func isMissingVolume(err error) bool {
	return err != nil && errors.Is(err, fs.ErrNotExist)
}

// isWrongPassword reports whether err is the symptom of a bad archive password
// (vs a structural/IO/format error). It gates whether the password loop should
// advance to the next candidate. rardecode's errBadPassword and sevenzip's
// errChecksum are unexported, so match their surfaced text: rardecode reports
// "incorrect password"; 7z surfaces a wrong AES key as a "checksum error".
func isWrongPassword(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "incorrect password") || strings.Contains(s, "checksum error")
}

// volumeSetName collapses a multi-volume member path to the set's base name
// (".../name.part01.rar" -> "name") for a compact worker-line label.
func volumeSetName(display string) string {
	base := filepath.Base(display)
	if m := newStyleRarVolume.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	return base
}

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
	// confirm (may be nil) is wired by newGatedSink. A stream reader calls it
	// (via ec.confirmPassword) once the password is proven -- i.e. the archive
	// decoded past its first member -- so the gate flushes what it buffered and
	// streams every later credential straight to the writer. That is what makes
	// the live "found" counter climb during a long extraction instead of jumping
	// at EOF, while still never leaking a wrong password's partial garbage.
	confirm func()
	p       *Progress
	// setStage (may be nil) publishes this worker's current archive stage to
	// the live TUI. Copied across recursion so nested members report too.
	setStage func(WorkerStage)
	// setItem (may be nil) publishes the provenance of the archive this worker
	// is currently inside (e.g. "outer.rar!inner.7z"). recurseNested re-points
	// it on descent and restores the parent on return so the live line names
	// the nested archive being worked on, not just the top-level item.
	setItem func(string)
	// sem (may be nil) is the engine-wide extraction budget (cap = worker
	// count). The owning worker already holds one slot; readZipFiles lends it
	// back (release on entry, reclaim on exit) while parked on a big zip's
	// member pool, and the members acquire from this same channel — so member
	// parallelism reuses the parked worker's core instead of adding to it. nil
	// disables member parallelism.
	sem chan struct{}
	// debug (may be nil) logs provenance-safe diagnostics (paths, counts,
	// elapsed) to the run's -debug log. Copied across recursion. Never carries
	// raw passwords or credential values.
	debug func(format string, args ...any)
	// hb (may be nil) throttles the "still extracting" heartbeat so a long
	// decode shows movement without flooding the log. Shared across recursion
	// (one per top-level archive tree).
	hb *debugThrottle
	// processor (may be nil -> defaultProcessor) turns a member's bytes into
	// findings. A seam for future scanners; today it is the ULP parser. Readers
	// call ec.parse, never ParseCredentials directly, so the seam stays in one
	// place.
	processor Processor
	// secrets (may be nil) receives non-credential member bytes for scanning.
	// secretMaxLen caps how much of each member is read (0 -> defaultSecretMaxLen).
	// Copied across recursion so nested members are scanned too.
	secrets      SecretSink
	secretMaxLen int64
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

// confirmPassword tells the gate the archive is proven decodable so it may flush
// and pass through. No-op for hermetic/unbuffered callers (nil confirm).
func (ec extractCtx) confirmPassword() {
	if ec.confirm != nil {
		ec.confirm()
	}
}

// countCredFile records one successfully parsed credential file: it bumps the
// scan's committed tally and ticks the live Progress counter so the TUI "files"
// number advances mid-extraction instead of jumping at archive EOF. Live ticks
// are atomic (parallel zip pool) and nil-safe (hermetic callers).
func (ec extractCtx) countCredFile(scan *archiveScan) {
	scan.files++
	ec.p.addFile()
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
	var credFiles, nestedFiles, otherFiles []*zipenc.File
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
		case ec.secrets != nil:
			// Members the credential path skips are where secrets live. Collect
			// them for a size-capped side scan; let them drive password
			// resolution (so a secrets-only archive still decrypts) but keep
			// them out of the byte scale, since they're read capped, not whole.
			otherFiles = append(otherFiles, f)
			if f.IsEncrypted() && (probe == nil || f.UncompressedSize64 < probe.UncompressedSize64) {
				probe = f
			}
			continue
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
	if len(credFiles) == 0 && len(nestedFiles) == 0 && len(otherFiles) == 0 {
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
	if n := len(credFiles) + len(nestedFiles); ec.parallelMembers(n) {
		// Surface the fan-out on the worker line: the byte bar carries the
		// motion, but the single panel row would otherwise read as one static
		// archive while every core is busy on its members. The member tasks
		// drop their item sink, so this label stands for the whole parallel run.
		ec.item(fmt.Sprintf("%s  ·  %d members in parallel", filepath.Base(ec.display), n))
		// Lend the owning worker's extraction slot to the member pool while it
		// is parked here, then reclaim it before returning to the worker loop
		// (which releases it). This keeps total in-flight extraction bounded by
		// the worker count instead of doubling it.
		<-ec.sem
		scan, err := readZipMembersParallel(ctx, credFiles, nestedFiles, ec, pw, cr)
		ec.sem <- struct{}{}
		if err == nil {
			scanOtherZipMembers(ctx, otherFiles, ec, pw)
		}
		return scan, err
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
	scanOtherZipMembers(ctx, otherFiles, ec, pw)
	return scan, nil
}

// scanOtherZipMembers feeds the archive's non-credential, non-archive members
// (the ones the credential path skips) to the secret sink. Best-effort: a
// member that fails to open or decrypt is skipped, never failing the archive.
// otherFiles is only ever populated when a sink is wired, so this is a no-op
// otherwise.
func scanOtherZipMembers(ctx context.Context, otherFiles []*zipenc.File, ec extractCtx, pw string) {
	for _, f := range otherFiles {
		if ctx.Err() != nil {
			return
		}
		if f.IsEncrypted() {
			f.SetPassword(pw)
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		ec.scanSecrets(ctx, rc, ec.display+"!"+f.Name)
		rc.Close()
	}
}

// memberFlushChunk caps how many zip members are buffered before they are
// merged and flushed. Members are processed in ordered chunks of this size so
// peak buffered credentials stay bounded to one chunk (not the whole archive),
// while output stays deterministic.
const memberFlushChunk = 256

// readZipMembersParallel reads a top-level zip's credential members and nested
// archives through the shared budget. Members are handled in ordered chunks:
// each chunk fans out (tasks collect into their own outcome slot, lock-free),
// then is merged in member-index order before the next chunk dispatches. This
// bounds buffered creds to one chunk and keeps emit order deterministic
// regardless of completion order. The credit (cr) is the only shared sink
// touched concurrently and is atomic. Per-task stage/item sinks are dropped so
// concurrent tasks never thrash the single worker slot; the archive-level
// "extracting" stage set by the caller stands.
func readZipMembersParallel(ctx context.Context, credFiles, nestedFiles []*zipenc.File, ec extractCtx, pw string, cr *creditor) (archiveScan, error) {
	n := len(credFiles) + len(nestedFiles)
	var members atomic.Int64
	var scan archiveScan
	for start := 0; start < n; start += memberFlushChunk {
		if err := ctx.Err(); err != nil {
			return scan, err
		}
		end := start + memberFlushChunk
		if end > n {
			end = n
		}
		out := make([]memberOutcome, end-start)
		boundedForEach(ctx, ec.sem, end-start, func(j int) {
			i := start + j
			if ctx.Err() != nil {
				out[j].ctxErr = ctx.Err()
				return
			}
			ec.heartbeat(int(members.Add(1)))
			if i < len(credFiles) {
				out[j] = readZipCredMember(ctx, credFiles[i], ec, pw, cr)
				return
			}
			out[j] = readZipNestedMember(ctx, nestedFiles[i-len(credFiles)], ec, pw, cr)
		})
		for j := range out {
			if out[j].ctxErr != nil {
				return scan, out[j].ctxErr
			}
			for _, is := range out[j].issues {
				ec.onIssue(is.path, is.kind, is.err)
			}
			scan.add(out[j].scan)
			for _, c := range out[j].creds {
				ec.emit(c)
			}
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
	creds, parseErr := ec.parse(countingReader{r: rc, c: cr}, name)
	closeErr := rc.Close()
	if parseErr != nil || closeErr != nil {
		o.issues = append(o.issues, pendingIssue{name, IssueParseError, firstErr(parseErr, closeErr)})
		return o
	}
	ec.countCredFile(&o.scan)
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

// gatedSink withholds a streaming pass's output only until the password is
// proven, then flushes it and passes everything after straight to the real
// sinks. rar/7z can't stream blindly: a wrong password fails mid-decode, so
// emitting eagerly would leak garbage. But holding the WHOLE archive back means
// the live "found" counter sits at 0 for minutes then jumps at EOF. The gate
// splits the difference: buffer up to the first proven member (confirm()), then
// stream. All methods run on the single stream goroutine, so the plain slices
// and bool need no locking; the real sinks it wraps have the same invariant.
type gatedSink struct {
	realEmit  func(Credential)
	realIssue func(path string, kind IssueKind, err error)
	creds     []Credential
	issues    []pendingIssue
	confirmed bool
}

func (g *gatedSink) emit(c Credential) {
	if g.confirmed {
		g.realEmit(c)
		return
	}
	g.creds = append(g.creds, c)
}

func (g *gatedSink) issue(p string, k IssueKind, e error) {
	if g.confirmed {
		g.realIssue(p, k, e)
		return
	}
	g.issues = append(g.issues, pendingIssue{p, k, e})
}

// confirm flushes what was held and flips to pass-through. Idempotent, so a
// reader can call it at every member boundary without tracking prior calls. An
// unconfirmed gate (wrong password) is simply dropped, discarding its buffer.
func (g *gatedSink) confirm() {
	if g.confirmed {
		return
	}
	g.confirmed = true
	for _, c := range g.creds {
		g.realEmit(c)
	}
	for _, is := range g.issues {
		g.realIssue(is.path, is.kind, is.err)
	}
	g.creds, g.issues = nil, nil
}

// newGatedSink wraps ec so emit/onIssue route through a fresh gate and confirm
// drives it. Returns the wrapped ec (pass it to the stream reader) and the gate.
func newGatedSink(ec extractCtx) (extractCtx, *gatedSink) {
	g := &gatedSink{realEmit: ec.emit, realIssue: ec.onIssue}
	ec.emit = g.emit
	ec.onIssue = g.issue
	ec.confirm = g.confirm
	return ec, g
}

// processSpilled runs the recursive reader over an already-spilled nested-archive
// temp and records the result into o. slot>=0 is a pooled run that owns its own
// panel row (sinks bound to the slot); slot<0 is an inline run on the producer
// goroutine, which borrows the producer's row for the nested archive and restores
// it on return. It always removes tmp and isolates every non-ctx failure as an
// issue on o, so one bad nested archive never aborts the parent.
func processSpilled(ctx context.Context, ec extractCtx, slot int, tmp, name string, o *memberOutcome) {
	defer os.Remove(tmp)
	display := ec.display + "!" + name
	child := ec
	child.depth++
	child.display = display
	child.emit = func(c Credential) { o.creds = append(o.creds, c) }
	child.onIssue = func(p string, k IssueKind, e error) { o.issues = append(o.issues, pendingIssue{p, k, e}) }
	if slot >= 0 {
		// Pooled child: its own row, independent of the producer's.
		child.setStage, child.setItem = slotSinks(ec.p, slot)
		ec.p.setActive(slot, display, StageExtracting)
	} else {
		// Inline child: borrow the producer's row for the nested archive, then
		// restore the parent label (and extracting stage) on return.
		ec.item(display)
		defer func() {
			ec.item(ec.display)
			ec.stage(StageExtracting)
		}()
	}
	scan, err := readArchiveCredentials(ctx, tmp, child, 0)
	if err != nil {
		if ctx.Err() != nil {
			o.ctxErr = err
			return
		}
		kind := IssueParseError
		if errors.Is(err, errPasswordNotFound) {
			kind = IssuePasswordNotFound
		}
		o.issues = append(o.issues, pendingIssue{display, kind, err})
		return
	}
	scan.nestedArchives++
	o.scan = scan
}

// spillAndDispatch handles one nested-archive member found while streaming a rar.
// The spill must run on the (forward-only) stream goroutine before the next
// rr.Next(); the expensive recursive processing is then offloaded to the pool
// when a budget slot is free, else run inline. The outcome is appended in stream
// order so the EOF merge stays deterministic. spillCr (nil for rar, whose bytes
// the caller credits) credits the spill copy.
func spillAndDispatch(ctx context.Context, ec extractCtx, wg *sync.WaitGroup, outcomes *[]*memberOutcome, body io.Reader, name string, spillCr *creditor) error {
	display := ec.display + "!" + name
	o := &memberOutcome{}
	*outcomes = append(*outcomes, o)
	if ec.depth+1 > maxArchiveDepth {
		o.issues = append(o.issues, pendingIssue{display, IssueParseError, fmt.Errorf("%w (limit %d)", errNestTooDeep, maxArchiveDepth)})
		// Drain so the stream stays aligned for the next member.
		_, _ = io.Copy(io.Discard, countingReader{r: body, c: spillCr})
		return nil
	}
	tmp, spillErr := spillToTemp(ec.tempDir, name, body, spillCr)
	if spillErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		o.issues = append(o.issues, pendingIssue{display, IssueParseError, spillErr})
		return nil
	}
	dispatchOrInline(ctx, ec, wg, func(slot int) {
		processSpilled(ctx, ec, slot, tmp, name, o)
	})
	return nil
}

// mergeOutcomes folds the per-member outcomes into the (buffered) sinks in stream
// order on the calling goroutine, so emit order stays deterministic regardless of
// which pool task finished first. A cancelled child short-circuits with its ctx
// error.
func mergeOutcomes(ec extractCtx, scan *archiveScan, outcomes []*memberOutcome) error {
	for _, o := range outcomes {
		if o.ctxErr != nil {
			return o.ctxErr
		}
		for _, is := range o.issues {
			ec.onIssue(is.path, is.kind, is.err)
		}
		scan.add(o.scan)
		for _, c := range o.creds {
			ec.emit(c)
		}
	}
	return nil
}

// rarFileProbe builds a first-member probe for a single-file rar: it opens the
// archive, decodes only the first file member's body, and reports whether the
// password is wrong. It never advances past the first member, so a probe race can
// resolve a password without re-streaming the whole archive.
func rarFileProbe(diskPath string) func(context.Context, string) bool {
	return func(ctx context.Context, pw string) bool {
		f, err := os.Open(diskPath)
		if err != nil {
			return false // can't open: not a password problem; let the full pass surface it
		}
		defer f.Close()
		unreg := registerAbort(ctx, f)
		defer unreg()
		rr, err := rardecode.NewReader(f, pw)
		if err != nil {
			return isWrongPassword(err)
		}
		return firstMemberRejects(func() (string, bool, error) {
			h, e := rr.Next()
			if e != nil {
				return "", false, e
			}
			return h.Name, h.IsDir, nil
		}, rr)
	}
}

// rarVolumeProbe builds the same first-member probe for a multi-volume set,
// following the on-disk volume sequence from the first part.
func rarVolumeProbe(first string) func(context.Context, string) bool {
	return func(ctx context.Context, pw string) bool {
		rc, err := rardecode.OpenReader(first, pw)
		if err != nil {
			return isWrongPassword(err)
		}
		defer rc.Close()
		return firstMemberRejects(func() (string, bool, error) {
			h, e := rc.Next()
			if e != nil {
				return "", false, e
			}
			return h.Name, h.IsDir, nil
		}, rc)
	}
}

// firstMemberRejects advances to the first non-dir member and decodes its body,
// reporting true only when the failure is a wrong password. EOF (empty/dir-only)
// and structural errors (e.g. a truncated volume set) are *not* password
// problems, so they return false and the caller's full pass handles them.
func firstMemberRejects(next func() (name string, isDir bool, err error), body io.Reader) bool {
	for {
		_, isDir, err := next()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			return isWrongPassword(err)
		}
		if isDir {
			continue
		}
		_, cerr := io.Copy(io.Discard, body)
		return isWrongPassword(cerr)
	}
}

// readRarCredentials extracts a single-file rar. The first candidate password
// (always "" for an unencrypted archive — the common case) is tried as one full
// pass; only if it is a wrong password are the remaining candidates resolved in
// parallel via a first-member probe race, then exactly one more full pass runs
// with the winner. The archive body is therefore streamed at most twice, never
// once per candidate.
func readRarCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	// One creditor for the whole item: it clamps to weight, so a wrong-password
	// first pass plus the winner pass never over-credit progress.
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	candidates := ec.passwords
	if len(candidates) == 0 {
		return archiveScan{}, fmt.Errorf("%w: no candidates", errPasswordNotFound)
	}
	scan, err := extractRarOnce(ctx, ec, diskPath, candidates[0], cr, true)
	if err == nil || ctx.Err() != nil || !isWrongPassword(err) {
		return scan, err // committed, cancelled, or a structural error a retry can't fix
	}
	rest := candidates[1:]
	if len(rest) == 0 {
		return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, err)
	}
	ec.debugf("archive %s: first password wrong, racing %d candidate(s) on first member", ec.display, len(rest))
	winner, ok := raceProbe(ctx, ec, rest, rarFileProbe(diskPath))
	if ctx.Err() != nil {
		return archiveScan{}, ctx.Err()
	}
	if !ok {
		return archiveScan{}, fmt.Errorf("%w: all %d candidates rejected", errPasswordNotFound, len(candidates))
	}
	return extractRarOnce(ctx, ec, diskPath, winner, cr, false)
}

// extractRarOnce runs one full streaming pass of a single-file rar with pw,
// buffering creds/issues and committing only on a clean EOF. first marks the
// initial attempt (for log wording). nil means committed; a non-nil error is
// classified by the caller (wrong password -> resolve the rest; otherwise stop).
func extractRarOnce(ctx context.Context, ec extractCtx, diskPath, pw string, cr *creditor, first bool) (archiveScan, error) {
	if first {
		ec.debugf("archive %s: extracting", ec.display)
	} else {
		ec.debugf("archive %s: extracting with resolved password", ec.display)
	}
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
		return archiveScan{}, err
	}
	// The gate streams creds through once the password is proven (past the first
	// member); a wrong password fails before that, so its buffer is dropped here.
	gatedEc, _ := newGatedSink(ec)
	scan, streamErr := readRarStream(ctx, gatedEc, rr)
	unreg()
	_ = f.Close()
	switch {
	case streamErr == nil:
		return scan, nil // creds already streamed to the writer
	case ctx.Err() != nil:
		return archiveScan{}, ctx.Err()
	case isWrongPassword(streamErr):
		ec.debugf("archive %s: password rejected after %s", ec.display, time.Since(attemptStart).Round(time.Millisecond))
		return scan, streamErr
	default:
		ec.debugf("archive %s: extraction failed after %s: %v",
			ec.display, time.Since(attemptStart).Round(time.Millisecond), streamErr)
		return scan, streamErr
	}
}

// readRarStream walks a single-file rar's members on one (forward-only) stream
// goroutine: credential files parse and emit inline (cheap), nested archives
// spill inline then offload their recursive processing to the pool (or run
// inline if the pool is saturated), joining before a deterministic merge at EOF.
// The caller (extractRarOnce) wraps ec in a gatedSink, so creds emitted here are
// withheld only until ec.confirmPassword() fires at the first proven member
// boundary -- then they stream to the writer, never before the password proves.
func readRarStream(ctx context.Context, ec extractCtx, rr *rardecode.Reader) (archiveScan, error) {
	ec.stage(StageExtracting)
	var scan archiveScan
	var wg sync.WaitGroup
	var outcomes []*memberOutcome
	members := 0
	validated := false
	stream := func() error {
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h, err := rr.Next()
			// Crossing a member boundary cleanly (next header or EOF) proves the
			// prior member decoded, so the password is right: let the gate flush
			// and stream from here. A non-EOF error means the prior member failed
			// to decode (wrong password) -- do not confirm.
			if members > 0 && (err == nil || errors.Is(err, io.EOF)) {
				ec.confirmPassword()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if h.IsDir {
				continue
			}
			members++
			ec.heartbeat(members)
			// Force the first member's body through the decoder so a wrong
			// password fails on its first CRC check, instead of (for solid
			// archives) decompressing every member up to the first credential
			// file. Members the switch already reads in full need no separate
			// drain.
			if !validated {
				validated = true
				if !isArchiveFile(h.Name) && !isPasswordFile(h.Name) {
					if _, derr := io.Copy(io.Discard, rr); derr != nil {
						return derr
					}
					continue
				}
			}
			switch {
			case isArchiveFile(h.Name):
				// Spill the current entry now (before the next Next()); bytes are
				// counted by the outer file reader, so the spill creditor is nil.
				if derr := spillAndDispatch(ctx, ec, &wg, &outcomes, rr, h.Name, nil); derr != nil {
					return derr
				}
			case isPasswordFile(h.Name):
				// Emit inline in stream order: pre-confirm the gate buffers these,
				// post-confirm they stream straight to the writer so the live
				// found-counter climbs. Nested archives stay on the outcome path
				// (merged at EOF) since they finish out of order.
				creds, perr := ec.parse(rr, ec.display+"!"+h.Name)
				if perr != nil {
					return perr
				}
				ec.countCredFile(&scan)
				for _, c := range creds {
					ec.emit(c)
				}
			}
		}
	}
	streamErr := stream()
	// Wait for dispatched children before touching outcomes, even on error, so no
	// goroutine writes to an outcome after we return.
	wg.Wait()
	if mergeErr := mergeOutcomes(ec, &scan, outcomes); mergeErr != nil && streamErr == nil {
		streamErr = mergeErr
	}
	return scan, streamErr
}

// readRarVolumes reads a new-style multi-volume RAR set (name.part1.rar,
// .part2.rar, ...). Unlike the single-file path it uses rardecode.OpenReader,
// which follows the on-disk volume sequence by name, so members that span
// volume boundaries decode correctly. Progress is credited per completed
// volume (coarser than the byte-accurate single-file path, but this is the rare
// case); finish() tops up any remainder. Like the single-file path it buffers
// credentials and commits only after a clean EOF so a wrong password yields no
// partial output.
// readRarVolumes reads a new-style multi-volume RAR set (name.part1.rar, ...).
// It uses the same try-first-then-race-the-rest password strategy as the
// single-file path (so the set is streamed at most twice, never once per
// candidate) and salvages a truncated set by committing the parts that decoded.
func readRarVolumes(ctx context.Context, volumes []string, ec extractCtx, weight int64) (archiveScan, error) {
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	first := volumes[0]
	total := len(volumes)
	candidates := ec.passwords
	if len(candidates) == 0 {
		return archiveScan{}, fmt.Errorf("%w: no candidates", errPasswordNotFound)
	}
	scan, err := extractRarVolumesOnce(ctx, ec, first, candidates[0], cr, total, true)
	if err == nil || ctx.Err() != nil || !isWrongPassword(err) {
		return scan, err
	}
	rest := candidates[1:]
	if len(rest) == 0 {
		return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, err)
	}
	ec.debugf("archive %s: first password wrong, racing %d candidate(s) on first member (multi-volume)", ec.display, len(rest))
	winner, ok := raceProbe(ctx, ec, rest, rarVolumeProbe(first))
	if ctx.Err() != nil {
		return archiveScan{}, ctx.Err()
	}
	if !ok {
		return archiveScan{}, fmt.Errorf("%w: all %d candidates rejected", errPasswordNotFound, len(candidates))
	}
	return extractRarVolumesOnce(ctx, ec, first, winner, cr, total, false)
}

// extractRarVolumesOnce runs one full pass over a volume set with pw. Like the
// single-file pass it streams creds through a gatedSink (held only until the
// password proves, then live); it additionally treats a missing trailing volume
// as a salvageable skip (keep what decoded, flag the gap) rather than a failure.
func extractRarVolumesOnce(ctx context.Context, ec extractCtx, first, pw string, cr *creditor, total int, firstAttempt bool) (archiveScan, error) {
	if firstAttempt {
		ec.debugf("archive %s: extracting (multi-volume, %d parts)", ec.display, total)
	} else {
		ec.debugf("archive %s: extracting with resolved password (multi-volume, %d parts)", ec.display, total)
	}
	attemptStart := time.Now()
	rc, err := rardecode.OpenReader(first, pw)
	if err != nil {
		return archiveScan{}, err
	}
	// The gate streams creds through once the password is proven; keep its handle
	// so the salvage path can force a flush when a truncation error interrupts
	// the normal boundary-confirm.
	gatedEc, gate := newGatedSink(ec)
	scan, streamErr := readRarVolumeStream(ctx, gatedEc, rc, cr, total)
	nvol := len(rc.Volumes())
	_ = rc.Close()
	switch {
	case streamErr == nil:
		return scan, nil // creds already streamed to the writer
	case ctx.Err() != nil:
		return archiveScan{}, ctx.Err()
	case isMissingVolume(streamErr):
		// Truncated set: every part on disk decoded cleanly and only a trailing
		// volume is absent. The missing-volume error is what stopped the stream,
		// so the last boundary-confirm may not have fired -- flush explicitly to
		// keep the creds read before the gap, then flag it.
		gate.confirm()
		ec.onIssue(ec.display, IssueMissingVolume,
			fmt.Errorf("%w: next volume missing after %d part(s)", errIncompleteVolumeSet, nvol))
		ec.debugf("archive %s: incomplete set, next volume missing after %d/%d part(s); kept creds from parts read",
			ec.display, nvol, total)
		return scan, nil
	case isWrongPassword(streamErr):
		ec.debugf("archive %s: password rejected after %s", ec.display, time.Since(attemptStart).Round(time.Millisecond))
		return scan, streamErr
	default:
		ec.debugf("archive %s: extraction failed after %s: %v",
			ec.display, time.Since(attemptStart).Round(time.Millisecond), streamErr)
		return scan, streamErr
	}
}

// readRarVolumeStream mirrors readRarStream for the OpenReader (multi-volume)
// path: it credits progress per member (PackedSize, since OpenReader hides the
// per-volume file reads), advances the "part N/M" worker label, and offloads
// nested-archive processing to the pool the same way.
func readRarVolumeStream(ctx context.Context, ec extractCtx, rc *rardecode.ReadCloser, cr *creditor, total int) (archiveScan, error) {
	ec.stage(StageExtracting)
	setName := volumeSetName(ec.display)
	var scan archiveScan
	var wg sync.WaitGroup
	var outcomes []*memberOutcome
	members := 0
	validated := false
	stream := func() error {
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h, err := rc.Next()
			// Re-point the worker line at the current volume so the row advances
			// "part 1/6 -> 2/6 ..." instead of reading as part01 for the whole
			// set. Re-asserted every iteration since an inline nested member
			// restores the parent label on return.
			if cur := len(rc.Volumes()); cur > 0 {
				ec.item(fmt.Sprintf("%s  ·  part %d/%d", setName, cur, total))
			}
			// Crossing a member boundary cleanly proves the prior member decoded
			// (right password): let the gate flush and stream from here. A non-EOF
			// error (wrong password, or a genuinely missing volume) skips confirm;
			// the salvage path flushes explicitly for the missing-volume case.
			if members > 0 && (err == nil || errors.Is(err, io.EOF)) {
				ec.confirmPassword()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if h.IsDir {
				continue
			}
			members++
			ec.heartbeat(members)
			// Credit this member's on-disk (packed) bytes once accounted for, so
			// the bar advances per member instead of jumping per ~GB volume;
			// finish() tops the small remainder to 100%.
			if !validated {
				validated = true
				if !isArchiveFile(h.Name) && !isPasswordFile(h.Name) {
					if _, derr := io.Copy(io.Discard, rc); derr != nil {
						return derr
					}
					cr.add(h.PackedSize)
					continue
				}
			}
			switch {
			case isArchiveFile(h.Name):
				if derr := spillAndDispatch(ctx, ec, &wg, &outcomes, rc, h.Name, nil); derr != nil {
					return derr
				}
			case isPasswordFile(h.Name):
				// Emit inline in stream order (see readRarStream); nested archives
				// stay on the outcome path merged at EOF.
				creds, perr := ec.parse(rc, ec.display+"!"+h.Name)
				if perr != nil {
					return perr
				}
				ec.countCredFile(&scan)
				for _, c := range creds {
					ec.emit(c)
				}
			}
			cr.add(h.PackedSize)
		}
	}
	streamErr := stream()
	wg.Wait()
	if mergeErr := mergeOutcomes(ec, &scan, outcomes); mergeErr != nil && streamErr == nil {
		streamErr = mergeErr
	}
	return scan, streamErr
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
// candidate is tried in a single pass; a gatedSink withholds creds until the
// first member proves the password, then streams the rest live (so hits climb),
// while a wrong password (which fails before proof) still yields no output.
func readSevenZip(ctx context.Context, ec extractCtx, weight int64, open func(pw string) (*sevenzip.Reader, func() error, error)) (archiveScan, error) {
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	var lastErr error
	emptyArchive := false
passwordLoop:
	for i, pw := range ec.passwords {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.stage(StageTestingPassword)
		if i == 0 {
			ec.debugf("archive %s: extracting", ec.display)
		} else {
			ec.debugf("archive %s: testing password %d/%d", ec.display, i+1, len(ec.passwords))
		}
		attemptStart := time.Now()
		zr, closeReader, err := open(pw)
		if err != nil {
			// Header-encrypted wrong password surfaces here as a checksum error;
			// anything else is a format/IO error a different password won't fix.
			if !isWrongPassword(err) {
				return archiveScan{}, err
			}
			lastErr = err
			ec.debugf("archive %s: password %d/%d rejected after %s: %v",
				ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond), err)
			continue
		}

		// Gate this attempt: creds buffer until the first cred member proves the
		// password (readSevenZipMembers confirms), then stream so hits climb. A
		// wrong password fails before confirm, so the gate's buffer is dropped.
		gatedEc, gate := newGatedSink(ec)
		scan, hadMembers, streamErr := readSevenZipMembers(ctx, gatedEc, zr, cr)
		_ = closeReader()
		switch {
		case streamErr == nil:
			if !hadMembers {
				emptyArchive = true
				break passwordLoop
			}
			gate.confirm() // flush any tail not yet streamed (e.g. nested-only sets)
			return scan, nil
		case ctx.Err() != nil:
			return archiveScan{}, ctx.Err()
		case isWrongPassword(streamErr):
			lastErr = streamErr
			ec.debugf("archive %s: password %d/%d incorrect after %s",
				ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond))
			continue
		default:
			// Non-password decode/IO error: stop instead of re-reading per password.
			ec.debugf("archive %s: extraction failed after %s: %v",
				ec.display, time.Since(attemptStart).Round(time.Millisecond), streamErr)
			return scan, streamErr
		}
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
	// Map this archive's uncompressed bytes onto its on-disk weight up front (the
	// 7z central directory is available without reading content), so crediting
	// each member's decoded reads moves the bar smoothly instead of leaving it at
	// 0 until finish() jumps it to 100%.
	var uncompressed int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if isArchiveFile(f.Name) || isPasswordFile(f.Name) {
			uncompressed += f.FileInfo().Size()
		}
	}
	cr.useScale(uncompressed)
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
		creds, parseErr := ec.parse(countingReader{r: rc, c: cr}, ec.display+"!"+member.Name)
		closeErr := rc.Close()
		if parseErr != nil || closeErr != nil {
			return scan, hadMembers, firstErr(parseErr, closeErr)
		}
		ec.countCredFile(&scan)
		for _, c := range creds {
			ec.emit(c)
		}
		// A cleanly-decoded credential member proves the password (content
		// members fail a wrong password on the first read): flush the gate and
		// stream subsequent creds live. Nested-only members don't confirm here --
		// recurseNested swallows their decode errors -- so readSevenZip flushes
		// the tail on clean EOF instead.
		ec.confirmPassword()
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
