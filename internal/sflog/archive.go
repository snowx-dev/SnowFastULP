package sflog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
	zipenc "github.com/yeka/zip"
)

// maxArchiveDepth caps how deep we recurse into archives-within-archives. The
// outer archive is depth 0; nested members may go up to this many levels. It
// guards against zip bombs and pathological nesting.
const maxArchiveDepth = 3

const (
	maxNestedArchiveMemberBytes = int64(512 << 20)
	maxNestedArchiveTreeBytes   = int64(2 << 30)
)

// errNestTooDeep is recorded (never fatal) when a nested archive exceeds
// maxArchiveDepth.
var (
	errNestTooDeep          = errors.New("archive nesting too deep")
	errNestedArchiveOverCap = errors.New("nested archive exceeds spill limit")
	errSevenZipMemberCRC    = errors.New("sevenzip member checksum mismatch")
	errDecoderPanic         = errors.New("archive decoder panic")
)

func decoderPanicError(value any) error {
	return fmt.Errorf("%w: %v", errDecoderPanic, value)
}

func recoverAsError(errp *error, ec extractCtx) {
	if value := recover(); value != nil {
		err := decoderPanicError(value)
		*errp = err
		ec.debugf("archive %s: decoder panic: %v", ec.display, value)
	}
}

func recoverAsIssue(o *memberOutcome, ec extractCtx, path string) {
	if value := recover(); value != nil {
		err := decoderPanicError(value)
		o.issues = append(o.issues, pendingIssue{path, IssueParseError, err})
		ec.debugf("archive %s: decoder panic: %v", path, value)
	}
}

type spillBudget struct {
	memberLimit int64
	treeLimit   int64
	used        atomic.Int64
}

func newSpillBudget(memberLimit, treeLimit int64) *spillBudget {
	if memberLimit <= 0 {
		memberLimit = maxNestedArchiveMemberBytes
	}
	if treeLimit <= 0 {
		treeLimit = maxNestedArchiveTreeBytes
	}
	return &spillBudget{memberLimit: memberLimit, treeLimit: treeLimit}
}

func (b *spillBudget) reserve(n int64) bool {
	if b == nil {
		return true
	}
	for {
		used := b.used.Load()
		if n < 0 || used > b.treeLimit-n {
			return false
		}
		if b.used.CompareAndSwap(used, used+n) {
			return true
		}
	}
}

func (b *spillBudget) release(n int64) {
	if b != nil && n > 0 {
		b.used.Add(-n)
	}
}

type spillWriter struct {
	w      io.Writer
	budget *spillBudget
	used   int64
}

func (w *spillWriter) Write(p []byte) (int, error) {
	if w.budget != nil && w.used > w.budget.memberLimit-int64(len(p)) {
		return 0, errNestedArchiveOverCap
	}
	if w.budget != nil && !w.budget.reserve(int64(len(p))) {
		return 0, errNestedArchiveOverCap
	}
	n, err := w.w.Write(p)
	if n > 0 {
		w.used += int64(n)
	}
	if w.budget != nil && n < len(p) {
		w.budget.release(int64(len(p) - n))
	}
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func drainContext(ctx context.Context, r io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := io.Copy(io.Discard, contextReader{ctx: ctx, r: r})
	return err
}

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
// advance to the next candidate. rardecode exposes ErrBadPassword; 7z still
// surfaces a wrong AES key as a checksum error. RAR4 body checksum failures
// are classified by isRarWrongPassword only when the member is encrypted.
func isWrongPassword(err error) bool {
	if err == nil {
		return false
	}
	var readErr *sevenzip.ReadError
	if errors.As(err, &readErr) && readErr.Encrypted {
		return true
	}
	if errors.Is(err, rardecode.ErrBadPassword) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "incorrect password") ||
		strings.Contains(s, "checksum error")
}

func isRarWrongPassword(err error, encrypted bool) bool {
	if !encrypted {
		return false
	}
	if isWrongPassword(err) {
		return true
	}
	return errors.Is(err, rardecode.ErrBadFileChecksum) ||
		errors.Is(err, rardecode.ErrCorruptPPM) ||
		errors.Is(err, rardecode.ErrHuffDecodeFailed)
}

func classifyRarReadError(err error, encrypted bool) error {
	if err != nil && isRarWrongPassword(err, encrypted) {
		return fmt.Errorf("rardecode: incorrect password: %w", err)
	}
	return err
}

// isRarDecoderFailure reports whether err is a rardecode decode-level failure
// (checksum, corrupt model, truncation, ...) rather than a content problem the
// parser found inside a successfully decoded member. Decode-level failures end
// the streaming attempt so the caller classifies them (password retry vs.
// structural error); plain content parse failures stay isolated per member.
func isRarDecoderFailure(err error) bool {
	if err == nil || isWrongPassword(err) {
		return true
	}
	return errors.Is(err, rardecode.ErrBadPassword) ||
		errors.Is(err, rardecode.ErrBadFileChecksum) ||
		errors.Is(err, rardecode.ErrCorruptPPM) ||
		errors.Is(err, rardecode.ErrHuffDecodeFailed) ||
		errors.Is(err, rardecode.ErrShortFile) ||
		errors.Is(err, rardecode.ErrInvalidFileBlock) ||
		errors.Is(err, rardecode.ErrUnexpectedArcEnd) ||
		errors.Is(err, rardecode.ErrUnknownVersion) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// isSevenZipDecoderFailure reports whether err came from the sevenzip decoder
// (open/decrypt/read/structural) rather than from parsing a successfully
// decoded member. Decoder failures end the password attempt; plain content
// parse failures stay isolated per member, like the zip member loop.
func isSevenZipDecoderFailure(err error) bool {
	if err == nil || isWrongPassword(err) {
		return true
	}
	var readErr *sevenzip.ReadError
	if errors.As(err, &readErr) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, fs.ErrNotExist)
}

// classifyRarMemberError decides whether a password-member parse failure must
// end the streaming attempt (non-nil error) or was an isolated content problem
// (nil; the caller records the issue and continues with the next member).
// Cancellation, wrong-password symptoms, decoder/structural failures, and a
// missing continuation volume (a truncated multi-volume set must reach the
// salvage path, not be swallowed as one member's parse issue) end the attempt;
// anything else (e.g. an oversized credential member) stays isolated so later
// members still extract.
func classifyRarMemberError(ctx context.Context, err error, encrypted bool) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if isRarWrongPassword(err, encrypted) {
		return classifyRarReadError(err, encrypted)
	}
	if isMissingVolume(err) {
		return err
	}
	if isRarDecoderFailure(err) {
		return err
	}
	return nil
}

func rarIntegrityError(err error) error {
	if cause := errors.Unwrap(err); cause != nil {
		return fmt.Errorf("rardecode: member integrity failure: %w", cause)
	}
	return fmt.Errorf("rardecode: member integrity failure: %w", err)
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
		if len(head) >= 4 && head[0] == 'P' && head[1] == 'K' {
			switch {
			case head[2] == 0x03 && head[3] == 0x04,
				head[2] == 0x05 && head[3] == 0x06,
				head[2] == 0x07 && head[3] == 0x08:
				return true, nil
			default:
				return false, nil
			}
		}
		// Not a zip header at offset 0: accept a self-extracting stub whose
		// archive is still real, i.e. a valid EOCD record within the final
		// 65,557 bytes (max comment + EOCD). Decoys fail both checks.
		return zipHasEOCD(f)
	}
	return true, nil
}

// zipEOCDLen is the fixed length of a zip end-of-central-directory record.
const zipEOCDLen = 22

// zipHasEOCD scans the final 65,557 bytes (EOCD record + maximum comment) for
// an end-of-central-directory record whose comment length fits the file and
// whose central-directory offset points inside it — the shape of a real,
// possibly SFX-prefixed zip. The scan mirrors the reader's own EOCD search, so
// anything it accepts the zip reader can open, and decoys fail here too.
func zipHasEOCD(f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := fi.Size()
	tail := int64(zipEOCDLen + 65_535)
	if tail > size {
		tail = size
	}
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	for i := len(buf) - zipEOCDLen; i >= 0; i-- {
		if buf[i] != 'P' || buf[i+1] != 'K' || buf[i+2] != 0x05 || buf[i+3] != 0x06 {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(buf[i+zipEOCDLen-2 : i+zipEOCDLen]))
		if i+zipEOCDLen+commentLen > len(buf) {
			continue
		}
		if cdOffset := int64(binary.LittleEndian.Uint32(buf[i+16 : i+20])); cdOffset < size {
			return true, nil
		}
	}
	return false, nil
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
	// winner (may be nil) is the run-shared sticky password: the last
	// password that opened an archive anywhere in this run. orderedPasswords()
	// puts it first, so sibling archives confirm a shared password with one
	// attempt instead of re-probing the whole list. The pointer is shared, the
	// struct is copied, so nested recursion keeps seeing it. nil disables
	// ordering (plain supplied order) — zero-value extractCtx in tests and
	// helpers keeps today's behavior.
	winner  *runWinner
	tempDir string // spill location for nested members, "" = os.TempDir()
	depth   int
	display string // logical provenance prefix, e.g. "a.zip!b.zip"
	emit    func(Credential)
	onIssue func(path string, kind IssueKind, err error)
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
	// env (may be nil) copies allowlisted members flat into the -env secrets
	// directory. The copier applies its own maxLen cap.
	env   *EnvCopier
	spill *spillBudget
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

func reportTdataIssue(ec extractCtx, err error) {
	if err == nil {
		return
	}
	if ec.env != nil && !errors.Is(err, errTdataOverCap) {
		ec.env.recordErrorSilent(EnvCopyTdataError, ec.display, err)
	}
	if ec.onIssue == nil {
		return
	}
	ec.onIssue(ec.display, IssueEnvCopy, fmt.Errorf("%s: %w", EnvCopyTdataError, err))
}

func promoteTdataOrReport(ec extractCtx, tg *tdataStager) {
	if err := tg.promote(ec.env); err != nil {
		if ec.onIssue != nil {
			ec.onIssue(ec.display, IssueEnvCopy, fmt.Errorf("%s: %w", EnvCopyTdataError, err))
		}
	}
}

// tdataStageStreamErr maps a staging error for streaming formats. Over-cap is
// an issue + skip (keep extracting ULPs); CRC/password still fail the stream.
func tdataStageStreamErr(ec extractCtx, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errTdataOverCap) {
		if ec.env != nil {
			ec.env.bumpSkippedTdataOverCap()
		}
		reportTdataIssue(ec, err)
		return nil
	}
	if errors.Is(err, errTdataPathCollision) {
		// A normalization collision means the staged tree is untrustworthy
		// (the stager already dropped the affected prefixes): report it as an
		// env-copy issue so HadIssue is set and -del keeps the source, but do
		// not fail the stream — remaining members still decode cleanly.
		reportTdataIssue(ec, err)
		return nil
	}
	return err
}

// countCredFile records one successfully parsed credential file: it bumps the
// scan's committed tally and ticks the live Progress counter so the TUI "files"
// number advances mid-extraction instead of jumping at archive EOF. Live ticks
// are atomic (parallel zip pool) and nil-safe (hermetic callers).
// replaySink emits a member sink's records through ec in order and then
// discards the sink; a replay/read error surfaces as an isolated parse issue
// on the member path so the rest of the archive is unaffected.
func replaySink(ec extractCtx, sink *credSink) {
	if sink == nil {
		return
	}
	if err := sink.replay(func(c Credential) error { ec.emit(c); return nil }); err != nil {
		ec.onIssue(sink.source, IssueParseError, err)
	}
	sink.discard()
}

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

// validationHeartbeatInterval is the cadence of the "still validating" lines
// emitted during the up-front 7z validation decode; a var so tests can shrink
// it. Matches the member-loop heartbeat's 5s convention.
var validationHeartbeatInterval = 5 * time.Second

// startValidationHeartbeat logs throttled "still validating" lines while the
// up-front 7z validation decode runs. The member-loop heartbeat does not
// cover this phase, so a long solid-archive validation would otherwise sit
// silent on the "testing password" stage and read as a hung worker (observed:
// 8.5 minutes with no output at all). Returns a stop function; calling it
// more than once is safe, and a nil debug sink yields a no-op stop.
func (ec extractCtx) startValidationHeartbeat(ctx context.Context, member string) func() {
	if ec.debug == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		start := time.Now()
		t := time.NewTicker(validationHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				ec.debug("archive %s: still validating %s (%s elapsed)",
					ec.display, member, time.Since(start).Round(time.Second))
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
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
	// sink holds the member's parsed credentials with bounded memory (small
	// in-memory head, temp-file spill beyond it); replay emits them in parse
	// order at the merge point, and discard drops them on failure.
	sink   *credSink
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
func readArchiveCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (scan archiveScan, err error) {
	defer recoverAsError(&err, ec)

	if ec.depth == 0 && ec.spill == nil {
		ec.spill = newSpillBudget(0, 0)
	}
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
func spillToTemp(ctx context.Context, tempDir, name string, rc io.Reader, cr *creditor, budget *spillBudget) (path string, used int64, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget == nil {
		budget = newSpillBudget(0, 0)
	}
	tf, err := os.CreateTemp(tempDir, "sfl-nested-*"+filepath.Ext(name))
	if err != nil {
		return "", 0, err
	}
	sw := &spillWriter{w: tf, budget: budget}
	// The member reader runs archive decoders in-process and can panic
	// mid-copy (the callers recover it as a decoder-panic error), so
	// close/remove/budget-release are armed immediately after allocation and
	// disarmed only when the spilled path is handed back whole. A panic must
	// not leak the open file, the temp file, or the spill budget.
	var copyErr error
	armed := true
	defer func() {
		if !armed {
			return
		}
		closeErr := tf.Close()
		_ = os.Remove(tf.Name())
		budget.release(sw.used)
		err = firstErr(copyErr, closeErr)
	}()
	_, copyErr = io.Copy(sw, countingReader{r: contextReader{ctx: ctx, r: rc}, c: cr})
	if copyErr != nil {
		return "", 0, copyErr
	}
	used = sw.used
	armed = false
	return tf.Name(), used, nil
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
	if ec.spill == nil {
		ec.spill = newSpillBudget(0, 0)
	}
	tmp, spilled, spillErr := spillToTemp(ctx, ec.tempDir, name, rc, spillCr, ec.spill)
	_ = rc.Close()
	if spillErr != nil {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.onIssue(display, IssueParseError, spillErr)
		return archiveScan{}, nil
	}
	defer os.Remove(tmp)
	defer ec.spill.release(spilled)

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
	if ec.depth == 0 && ec.spill == nil {
		ec.spill = newSpillBudget(0, 0)
	}
	var credFiles, nestedFiles, envOtherFiles, tdataFiles []*zipenc.File
	var probe *zipenc.File
	var uncompressed int64
	for _, f := range files {
		if f.FileInfo().IsDir() {
			continue
		}
		// A tdata-prefixed member belongs to a Telegram session tree we copy
		// as a unit; stage it and keep it out of the cred/env buckets
		// so it is not also flat-copied.
		if ec.env != nil {
			if _, _, ok := tdataMemberPrefix(f.Name); ok {
				tdataFiles = append(tdataFiles, f)
				// tdata members are read during the copy tail; keeping them out
				// of the denominator let the cred pass saturate the bar early.
				uncompressed += int64(f.UncompressedSize64)
				maybeEncryptedProbe(f, &probe)
				continue
			}
		}
		switch {
		case isArchiveFile(f.Name):
			nestedFiles = append(nestedFiles, f)
		case isPasswordFile(f.Name):
			credFiles = append(credFiles, f)
		case isEnvCopyCandidate(f.Name):
			// Env candidates are copied via envOtherFiles.
			envOtherFiles = append(envOtherFiles, f)
			// Copied during the copy tail; the denominator must include them.
			uncompressed += int64(f.UncompressedSize64)
			maybeEncryptedProbe(f, &probe)
			continue
		default:
			continue
		}
		uncompressed += int64(f.UncompressedSize64)
		// Probe with the *smallest* encrypted member so StageTestingPassword
		// stays a short blip even on a large (split) set, instead of decrypting
		// the first (possibly huge) member once per candidate password.
		maybeEncryptedProbe(f, &probe)
	}
	if len(credFiles) == 0 && len(nestedFiles) == 0 && len(envOtherFiles) == 0 && len(tdataFiles) == 0 {
		return archiveScan{}, nil
	}
	tdataFiles = filterConfirmedTdataZip(tdataFiles)
	tdataFiles = dropOverCapTdataZip(tdataFiles, ec)

	tg := newTdataStager(ec.env)
	defer tg.cleanup()

	// Resolve a single working password against the smallest encrypted member,
	// then reuse it for all members. yeka/zip handles WinZip AES and legacy
	// ZipCrypto.
	pw := ""
	if probe != nil {
		ec.stage(StageTestingPassword)
		ec.debugf("archive %s: resolving password against probe member %s (%dB uncompressed, %d candidate(s))",
			ec.display, filepath.Base(probe.Name), probe.UncompressedSize64, len(ec.passwords))
		resolved, ok, pwErr := resolveZipPassword(ctx, probe, ec.orderedPasswords())
		if pwErr != nil {
			// Only a context cancellation surfaces as the error; "not found"
			// stays the distinct password-not-found failure below.
			return archiveScan{}, pwErr
		}
		if !ok {
			return archiveScan{}, errPasswordNotFound
		}
		ec.winner.promote(resolved)
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
		// archive while every core is busy on its members. Member tasks publish
		// the decrement per completed member from readZipMembersParallel.
		ec.item(fmt.Sprintf("%s  ·  %d members left", filepath.Base(ec.display), n))
		// Lend the owning worker's extraction slot to the member pool while it
		// is parked here, then reclaim it before returning to the worker loop
		// (which releases it). This keeps total in-flight extraction bounded by
		// the worker count instead of doubling it.
		<-ec.sem
		scan, err := readZipMembersParallel(ctx, credFiles, nestedFiles, ec, pw, cr)
		ec.sem <- struct{}{}
		if err == nil && ctx.Err() == nil {
			if n := len(envOtherFiles) + len(tdataFiles); n > 0 {
				// Replace the members-left label (now at 0) with the copy-tail
				// count so the row keeps moving until the engine clears it.
				ec.item(fmt.Sprintf("%s  ·  copying %d file(s)", filepath.Base(ec.display), n))
			}
			copyOtherZipMembers(ctx, envOtherFiles, ec, pw, cr)
			if stageTdataZipMembers(ctx, tdataFiles, ec, tg, pw, cr) {
				promoteTdataOrReport(ec, tg)
			}
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
		replaySink(ec, o.sink)
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
	if n := len(envOtherFiles) + len(tdataFiles); n > 0 {
		// Replace the members-left label (now at 0) with the copy-tail count so
		// the row keeps moving until the engine clears it.
		ec.item(fmt.Sprintf("%s  ·  copying %d file(s)", filepath.Base(ec.display), n))
	}
	copyOtherZipMembers(ctx, envOtherFiles, ec, pw, cr)
	if ctx.Err() == nil && stageTdataZipMembers(ctx, tdataFiles, ec, tg, pw, cr) {
		promoteTdataOrReport(ec, tg)
	}
	return scan, nil
}

// filterConfirmedTdataZip drops tdata members whose prefix has no key_data*
// sibling in the zip listing, so decoy trees are never decompressed.
func filterConfirmedTdataZip(files []*zipenc.File) []*zipenc.File {
	if len(files) == 0 {
		return files
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	conf := tdataConfirmedPrefixes(names)
	var out []*zipenc.File
	for _, f := range files {
		p, _, ok := tdataMemberPrefix(f.Name)
		if ok && conf[p] {
			out = append(out, f)
		}
	}
	return out
}

// dropOverCapTdataZip drops confirmed tdata prefixes whose listed uncompressed
// size exceeds TdataCopyMaxBytes so we never decompress a disk-bomb tree.
func dropOverCapTdataZip(files []*zipenc.File, ec extractCtx) []*zipenc.File {
	if len(files) == 0 {
		return files
	}
	sizes := map[string]int64{}
	for _, f := range files {
		p, _, ok := tdataMemberPrefix(f.Name)
		if ok {
			sizes[p] += int64(f.UncompressedSize64)
		}
	}
	keep := map[string]bool{}
	skipped := false
	for p, n := range sizes {
		if n > TdataCopyMaxBytes {
			skipped = true
			if ec.env != nil {
				ec.env.bumpSkippedTdataOverCap()
			}
			continue
		}
		keep[p] = true
	}
	if skipped {
		reportTdataIssue(ec, errTdataOverCap)
	}
	var out []*zipenc.File
	for _, f := range files {
		p, _, ok := tdataMemberPrefix(f.Name)
		if ok && keep[p] {
			out = append(out, f)
		}
	}
	return out
}

// stageTdataZipMembers streams confirmed tdata zip members into the stager.
// cr (may be nil) credits the member bytes as they are read so the copy tail
// advances the same bar the cred pass used.
// Returns false if the context was cancelled mid-stage (caller must not promote).
func stageTdataZipMembers(ctx context.Context, tdataFiles []*zipenc.File, ec extractCtx, tg *tdataStager, pw string, cr *creditor) bool {
	if ec.env == nil || tg == nil || len(tdataFiles) == 0 {
		return true
	}
	ok := true
	for i, f := range tdataFiles {
		if ctx.Err() != nil {
			return false
		}
		ec.heartbeat(i + 1)
		member := f
		if member.IsEncrypted() {
			member.SetPassword(pw)
		}
		rc, err := member.Open()
		if err != nil {
			reportTdataIssue(ec, err)
			ok = false
			continue
		}
		_, stageErr := stageIfTdata(tg, member.Name, countingReader{r: rc, c: cr})
		rc.Close()
		if stageErr != nil {
			if errors.Is(stageErr, errTdataOverCap) {
				ec.env.bumpSkippedTdataOverCap()
				reportTdataIssue(ec, stageErr)
				continue
			}
			reportTdataIssue(ec, stageErr)
			ok = false
		}
	}
	return ok && ctx.Err() == nil
}

// copyOtherZipMembers copies env/key zip members flat into the -env secrets dir.
// cr (may be nil) credits the member bytes as they are read.
func copyOtherZipMembers(ctx context.Context, envFiles []*zipenc.File, ec extractCtx, pw string, cr *creditor) {
	if ec.env == nil {
		return
	}
	for i, f := range envFiles {
		if ctx.Err() != nil {
			return
		}
		ec.heartbeat(i + 1)
		member := f
		if member.IsEncrypted() {
			member.SetPassword(pw)
		}
		rc, err := member.Open()
		if err != nil {
			ec.env.recordError(EnvCopyOpenError, ec.display+"!"+member.Name, err)
			continue
		}
		ec.env.CopyMember(ctx, ec.display, member.Name, countingReader{r: rc, c: cr})
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
// touched concurrently and is atomic. Member tasks publish the "N members
// left" label per completion — safe because the production item sink is an
// atomic slot store; nested members' own sinks are dropped so they never
// publish, and the archive-level "extracting" stage set by the caller stands.
func readZipMembersParallel(ctx context.Context, credFiles, nestedFiles []*zipenc.File, ec extractCtx, pw string, cr *creditor) (archiveScan, error) {
	n := len(credFiles) + len(nestedFiles)
	var members atomic.Int64
	// publishedLeft floors the members-left label so out-of-order completions
	// never publish a higher count than one already shown.
	var publishedLeft atomic.Int64
	publishedLeft.Store(int64(n))
	base := filepath.Base(ec.display)
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
			} else {
				out[j] = readZipNestedMember(ctx, nestedFiles[i-len(credFiles)], ec, pw, cr)
			}
			// Per-member label update: the chunked merge only published every
			// memberFlushChunk members, which froze the counter for the whole
			// duration of a slow chunk. The production item sink is an atomic
			// slot store (Progress.setWorkerPath), so publishing from the pool
			// goroutines is race-free; the CAS floor keeps the displayed count
			// monotonic when completions publish out of order.
			if left := n - int(members.Load()); left > 0 {
				for {
					cur := publishedLeft.Load()
					if int64(left) >= cur {
						break
					}
					if publishedLeft.CompareAndSwap(cur, int64(left)) {
						ec.item(fmt.Sprintf("%s  ·  %d members left", base, left))
						break
					}
				}
			}
		})
		for j := range out {
			if out[j].ctxErr != nil {
				return scan, out[j].ctxErr
			}
			for _, is := range out[j].issues {
				ec.onIssue(is.path, is.kind, is.err)
			}
			scan.add(out[j].scan)
			replaySink(ec, out[j].sink)
		}
		// (Per-member label updates happen in the task closures above.)
	}
	return scan, nil
}

// runWinner remembers the last password that opened an archive anywhere in a
// run. Later archives try it first (orderedPasswords), so a password shared
// across archives is confirmed once instead of once per archive. All methods
// are nil-safe: a zero-value slot or a nil pointer keeps the plain order.
type runWinner struct {
	mu sync.Mutex
	pw string
}

func (w *runWinner) get() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pw
}

func (w *runWinner) promote(pw string) {
	if w == nil || pw == "" {
		return
	}
	w.mu.Lock()
	w.pw = pw
	w.mu.Unlock()
}

// orderedPasswords returns the archive-level candidate list with the run's
// winning password (if any archive has opened with one yet) first, then the
// supplied order. Only the top-level sweeps use this; recursion-level helpers
// that already hold a working pw keep using zipCandidatesOrdered(pw, ...).
// With no winner yet the supplied order is returned unchanged — notably the
// [""] candidate used for unencrypted archives, which zipCandidatesOrdered's
// current-dedup would otherwise drop.
func (ec extractCtx) orderedPasswords() []string {
	w := ec.winner.get()
	if w == "" {
		return ec.passwords
	}
	return zipCandidatesOrdered(w, ec.passwords)
}

// zipCandidatesOrdered lists the candidate passwords with `current` first (it
// is the probe's winner and usually decodes the member), then the rest in
// supplied order.
func zipCandidatesOrdered(current string, passwords []string) []string {
	out := make([]string, 0, len(passwords)+1)
	if current != "" {
		out = append(out, current)
	}
	for _, pw := range passwords {
		if pw != current {
			out = append(out, pw)
		}
	}
	return out
}

// zipCandidatesExcluding lists the candidate passwords with `skip` removed,
// preserving order. Used where `skip` is already known to fail the member.
func zipCandidatesExcluding(skip string, passwords []string) []string {
	out := make([]string, 0, len(passwords))
	for _, pw := range passwords {
		if pw != skip {
			out = append(out, pw)
		}
	}
	return out
}

// rarRaceOrder orders the fallback race candidates for the rar paths after the
// first full pass failed with a wrong password. The empty password can never
// prove an encrypted archive (firstMemberRejects returns false for an
// unencrypted first member), but it CAN win the probe on an archive whose
// first member is unencrypted while later members are encrypted — a doomed
// full pass. It therefore goes last, never in supplied position.
func rarRaceOrder(rest []string) []string {
	out := make([]string, 0, len(rest))
	for _, pw := range rest {
		if pw != "" {
			out = append(out, pw)
		}
	}
	for _, pw := range rest {
		if pw == "" {
			out = append(out, pw)
		}
	}
	return out
}

func zipContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// maybeResolveZipMemberPassword resolves f's own password when the
// probe-resolved `pw` does not decode it. ZIP permits per-entry passwords, so
// one probe result must not be assumed for every encrypted member. Returns the
// resolved password, whether one was found, and a context-only error.
func maybeResolveZipMemberPassword(ctx context.Context, f *zipenc.File, pw string, passwords []string) (string, bool, error) {
	f.SetPassword(pw)
	if err := probeBodyDecodes(ctx, f); err == nil {
		return pw, true, nil // pw decodes this member; the failure is content
	} else if zipContextErr(err) {
		return "", false, err
	}
	// pw is the wrong password for THIS member: sweep the remaining
	// candidates (pw already failed, so it is excluded).
	return resolveZipPassword(ctx, f, zipCandidatesExcluding(pw, passwords))
}

// readZipCredMember opens, decrypts and parses one credential member, returning
// a self-contained outcome (no shared sinks touched except the atomic cr). A bad
// member becomes an isolated issue and never discards the rest of the archive.
func readZipCredMember(ctx context.Context, f *zipenc.File, ec extractCtx, pw string, cr *creditor) (o memberOutcome) {
	name := ec.display + "!" + f.Name
	defer recoverAsIssue(&o, ec, name)
	o.sink = newCredSink(ec.tempDir, name)
	if f.IsEncrypted() {
		f.SetPassword(pw)
	}
	rc, err := f.Open()
	if err != nil && f.IsEncrypted() && !zipContextErr(err) {
		// Open validates the encryption header eagerly, so a member encrypted
		// under a different candidate fails HERE, before any parse. Resolve
		// the member's own password over the candidates and retry the open.
		if resolved, ok, rerr := maybeResolveZipMemberPassword(ctx, f, pw, ec.passwords); rerr != nil {
			o.ctxErr = rerr
			return o
		} else if ok {
			f.SetPassword(resolved)
			rc, err = f.Open()
		}
	}
	if err != nil {
		o.issues = append(o.issues, pendingIssue{name, IssueOpenError, err})
		return o
	}
	mixed, parseErr := func() (mixed bool, err error) {
		defer func() {
			if closeErr := rc.Close(); err == nil {
				err = closeErr
			}
		}()
		return ec.parse(countingReader{r: rc, c: cr}, name, o.sink.add)
	}()
	if parseErr != nil {
		// The failed attempt's partial records are dropped with the sink; a
		// retry starts from a clean one.
		o.sink.discard()
		o.sink = newCredSink(ec.tempDir, name)
	}
	if parseErr != nil && f.IsEncrypted() {
		resolved, ok, rerr := maybeResolveZipMemberPassword(ctx, f, pw, ec.passwords)
		if rerr != nil {
			o.ctxErr = rerr
			return o
		}
		if ok && resolved != pw {
			f.SetPassword(resolved)
			mixed, parseErr = func() (mixed bool, err error) {
				rc, err := f.Open()
				if err != nil {
					return false, err
				}
				defer func() {
					if closeErr := rc.Close(); err == nil {
						err = closeErr
					}
				}()
				return ec.parse(countingReader{r: rc, c: cr}, name, o.sink.add)
			}()
		}
	}
	if parseErr != nil {
		o.sink.discard()
		o.sink = nil
		o.issues = append(o.issues, pendingIssue{name, IssueParseError, parseErr})
		return o
	}
	if mixed {
		// Mixed member (review H-20): labeled precedence kept, but the
		// discarded colon lines are real credentials — flag the archive so
		// -del keeps it and history does not record it complete.
		o.issues = append(o.issues, pendingIssue{name, IssueMixedFormat, nil})
	}
	ec.countCredFile(&o.scan)
	return o
}

// readZipNestedMember recurses into one nested archive member, collecting its
// creds/issues into a task-local outcome. The live stage/item sinks are dropped
// so concurrent nested tasks don't fight over the single worker slot; the
// recursion runs sequentially (depth > 0, so parallelMembers is false).
func readZipNestedMember(ctx context.Context, f *zipenc.File, ec extractCtx, pw string, cr *creditor) (o memberOutcome) {
	o.sink = newCredSink(ec.tempDir, ec.display+"!"+f.Name)
	taskEc := ec
	taskEc.emit = func(c Credential) { _ = o.sink.add(c) }
	taskEc.onIssue = func(p string, k IssueKind, e error) { o.issues = append(o.issues, pendingIssue{p, k, e}) }
	taskEc.setStage = nil
	taskEc.setItem = nil
	member := f
	defer recoverAsIssue(&o, taskEc, ec.display+"!"+member.Name)
	if member.IsEncrypted() {
		// ZIP permits per-entry passwords: resolve this nested member's own
		// password against the candidates instead of assuming the probe's
		// winner. resolveZipPassword confirms by body-decode (same rules as
		// the probe), so garbage spills never reach the recursive reader. The
		// extra body decode is paid only by encrypted nested members.
		resolved, ok, rerr := resolveZipPassword(ctx, member, zipCandidatesOrdered(pw, ec.passwords))
		if rerr != nil {
			o.ctxErr = rerr // ctx only
			return o
		}
		if !ok {
			o.issues = append(o.issues, pendingIssue{ec.display + "!" + member.Name, IssuePasswordNotFound, errPasswordNotFound})
			return o
		}
		member.SetPassword(resolved)
	}
	open := func() (io.ReadCloser, error) {
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

// maybeEncryptedProbe records f as the password probe when it is encrypted and
// has a non-zero uncompressed size, picking the smallest such member. A 0-byte
// encrypted member is still skipped: its check byte can verify a password, but
// it can't confirm the resolved password decodes real member data, so a fluke
// pass would cascade into every real member failing as a parse error instead
// of the archive being flagged password-not-found.
func maybeEncryptedProbe(f *zipenc.File, probe **zipenc.File) {
	if !f.IsEncrypted() || f.UncompressedSize64 == 0 {
		return
	}
	if *probe == nil || f.UncompressedSize64 < (*probe).UncompressedSize64 {
		*probe = f
	}
}

// resolveZipPassword finds the first candidate that verifies against the
// (encrypted) probe member's encryption header AND whose body decodes. The
// header check (VerifyPassword: WinZip AES salt/PVV or the ZipCrypto 12-byte
// check byte) is probabilistic — a wrong ZipCrypto candidate matches the check
// byte 1/256 of the time and the AES PVV can collide — so each header match is
// confirmed by streaming the probe member through checksummed decompression. A
// body failure is a false positive: the sweep continues with the next
// candidate. pw is empty when no candidate verifies; the error is reserved for
// context cancellation so the caller can distinguish it from
// password-not-found.
func resolveZipPassword(ctx context.Context, m *zipenc.File, passwords []string) (string, bool, error) {
	for _, pw := range passwords {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		m.SetPassword(pw)
		if err := m.VerifyPassword(); err != nil {
			continue
		}
		if err := probeBodyDecodes(ctx, m); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", false, err
			}
			// Header false positive: its body failed checksum/decoding.
			continue
		}
		return pw, true, nil
	}
	return "", false, nil
}

// probeBodyDecodes streams the probe member's body through decompression to
// EOF and requires the checksum/decoder to succeed. Read chunks are bounded so
// context cancellation stops within one chunk. Returns nil only when the body
// decoded cleanly through EOF; a cancellation surfaces as its context error.
func probeBodyDecodes(ctx context.Context, m *zipenc.File) error {
	rc, err := m.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, rerr := rc.Read(buf)
		switch {
		case rerr == io.EOF:
			return nil
		case rerr != nil:
			return rerr
		}
	}
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
	o.sink = newCredSink(ec.tempDir, ec.display+"!"+name)
	defer os.Remove(tmp)
	display := ec.display + "!" + name
	defer recoverAsIssue(o, ec, display)
	child := ec
	child.depth++
	child.display = display
	child.emit = func(c Credential) { _ = o.sink.add(c) }
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
// when a budget slot is free, else run inline. solid is the member's rardecode
// Solid flag (a solid stream must be fully drained to keep the decoder
// aligned; a non-solid one is skipped by the next Next() reading only the
// remaining packed bytes), encrypted its rardecode Encrypted flag. spillCr
// (nil for rar, whose bytes the caller credits) credits the spill copy.
func spillAndDispatch(ctx context.Context, ec extractCtx, wg *sync.WaitGroup, outcomes *[]*memberOutcome, body io.Reader, name string, spillCr *creditor, solid, encrypted bool) error {
	display := ec.display + "!" + name
	o := &memberOutcome{}
	*outcomes = append(*outcomes, o)
	if ec.depth+1 > maxArchiveDepth {
		o.issues = append(o.issues, pendingIssue{display, IssueParseError, fmt.Errorf("%w (limit %d)", errNestTooDeep, maxArchiveDepth)})
		// Only solid streams must be drained so the decoder stays aligned for
		// the next member; non-solid archives skip the packed bytes on Next().
		if solid {
			_ = drainContext(ctx, countingReader{r: body, c: spillCr})
		}
		return nil
	}
	if ec.spill == nil {
		ec.spill = newSpillBudget(0, 0)
	}
	tmp, spilled, spillErr := spillToTemp(ctx, ec.tempDir, name, body, spillCr, ec.spill)
	if spillErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(spillErr, errNestedArchiveOverCap) {
			// Drain only solid streams to keep the decoder aligned; non-solid
			// archives skip the remaining packed bytes on the next Next()
			// without decompressing them.
			if solid {
				if err := drainContext(ctx, countingReader{r: body, c: spillCr}); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if encrypted && isRarWrongPassword(err, true) {
						return classifyRarReadError(err, true)
					}
				}
			}
			o.issues = append(o.issues, pendingIssue{display, IssueParseError, spillErr})
			return nil
		}
		// A body read failure inside an encrypted member is the wrong-password
		// symptom while the stream is still racing candidates: propagate the
		// classified error so the stream fails and readRarCredentials races the
		// remaining candidates, instead of recording a parse issue and
		// silently extracting a truncated nested archive.
		if encrypted && isRarWrongPassword(spillErr, true) {
			return classifyRarReadError(spillErr, true)
		}
		o.issues = append(o.issues, pendingIssue{display, IssueParseError, spillErr})
		return nil
	}
	dispatchOrInline(ctx, ec, wg, func(slot int) {
		defer ec.spill.release(spilled)
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
			for _, oo := range outcomes {
				oo.sink.discard()
			}
			return o.ctxErr
		}
		for _, is := range o.issues {
			ec.onIssue(is.path, is.kind, is.err)
		}
		scan.add(o.scan)
		replaySink(ec, o.sink)
	}
	return nil
}

// rarFileProbe builds a first-member probe for a single-file rar: it opens the
// archive, decodes only the first file member's body, and reports whether the
// password is wrong. It never advances past the first member, so a probe race can
// resolve a password without re-streaming the whole archive.
func rarFileProbe(diskPath string) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, pw string) (wrong bool, err error) {
		defer func() {
			if value := recover(); value != nil {
				wrong = false
				err = decoderPanicError(value)
			}
		}()
		f, err := os.Open(diskPath)
		if err != nil {
			return false, nil // can't open: not a password problem; let the full pass surface it
		}
		defer f.Close()
		unreg := registerAbort(ctx, f)
		defer unreg()
		rr, err := rardecode.NewReader(f, rardecode.Password(pw))
		if err != nil {
			return isWrongPassword(err), nil
		}
		return firstMemberRejects(func() (string, bool, bool, error) {
			h, e := rr.Next()
			if e != nil {
				return "", false, false, e
			}
			return h.Name, h.IsDir, h.Encrypted, nil
		}, rr), nil
	}
}

// rarVolumeProbe builds the same first-member probe for a multi-volume set,
// following the on-disk volume sequence from the first part.
func rarVolumeProbe(first string) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, pw string) (wrong bool, err error) {
		defer func() {
			if value := recover(); value != nil {
				wrong = false
				err = decoderPanicError(value)
			}
		}()
		rc, err := rardecode.OpenReader(first, rardecode.Password(pw))
		if err != nil {
			return isWrongPassword(err), nil
		}
		defer rc.Close()
		return firstMemberRejects(func() (string, bool, bool, error) {
			h, e := rc.Next()
			if e != nil {
				return "", false, false, e
			}
			return h.Name, h.IsDir, h.Encrypted, nil
		}, rc), nil
	}
}

// firstMemberRejects advances to the first non-dir member and decodes its body,
// reporting true only when the failure is a wrong password. EOF (empty/dir-only)
// and structural errors (e.g. a truncated volume set) are *not* password
// problems, so they return false and the caller's full pass handles them.
func firstMemberRejects(next func() (name string, isDir, encrypted bool, err error), body io.Reader) bool {
	for {
		_, isDir, encrypted, err := next()
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
		if cerr != nil && isRarWrongPassword(cerr, encrypted) {
			return true
		}
		if encrypted {
			return false
		}
	}
}

// readRarCredentials extracts a single-file rar. The first candidate password
// (always "" for an unencrypted archive — the common case) is tried as one full
// pass; only if it is a wrong password are the remaining candidates resolved in
// parallel via a first-member probe race, then exactly one more full pass runs
// with the winner. The archive body is therefore streamed at most twice, never
// once per candidate.
func readRarCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	// An old-style multi-volume set ("<name>.rar" + "<name>.rNN") must never
	// reach the single-file NewReader(io.Reader) path below: an io.Reader has
	// no pathname, so rardecode cannot open continuation volumes and only
	// volume 1 would decode. Route the set through the pathname-based
	// OpenReader path with the full set's weight.
	if set := oldStyleRarSet(diskPath); len(set) > 1 {
		var setWeight int64
		for _, p := range set {
			setWeight += fileWeight(p)
		}
		return readRarVolumes(ctx, set, ec, setWeight)
	}
	// One creditor for the whole item: it clamps to weight, so a wrong-password
	// first pass plus the winner pass never over-credit progress.
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	candidates := ec.orderedPasswords()
	if len(candidates) == 0 {
		return archiveScan{}, fmt.Errorf("%w: no candidates", errPasswordNotFound)
	}
	scan, err := extractRarOnce(ctx, ec, diskPath, candidates[0], cr, true)
	if err == nil {
		ec.winner.promote(candidates[0])
	}
	if err == nil || ctx.Err() != nil || !isWrongPassword(err) {
		return scan, err // committed, cancelled, or a structural error a retry can't fix
	}
	rest := rarRaceOrder(append([]string(nil), candidates[1:]...))
	for len(rest) > 0 {
		ec.debugf("archive %s: first password wrong, racing %d candidate(s) on first member", ec.display, len(rest))
		winner, ok, probeErr := raceProbe(ctx, ec, rest, rarFileProbe(diskPath))
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		if probeErr != nil {
			return archiveScan{}, probeErr
		}
		if !ok {
			break
		}
		scan, err = extractRarOnce(ctx, ec, diskPath, winner, cr, false)
		if err == nil {
			ec.winner.promote(winner)
		}
		if err == nil || ctx.Err() != nil || !isWrongPassword(err) {
			return scan, err
		}
		next := rest[:0]
		for _, candidate := range rest {
			if candidate != winner {
				next = append(next, candidate)
			}
		}
		rest = next
	}
	return archiveScan{}, fmt.Errorf("%w: all %d candidates rejected", errPasswordNotFound, len(candidates))
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
	defer func() {
		unreg()
		_ = f.Close()
	}()
	rr, err := rardecode.NewReader(countingReader{r: f, c: cr}, rardecode.Password(pw))
	if err != nil {
		return archiveScan{}, err
	}
	// The gate streams creds through once the password is proven (past the first
	// member); a wrong password fails before that, so its buffer is dropped here.
	gatedEc, gate := newGatedSink(ec)
	scan, streamErr := readRarStream(ctx, gatedEc, rr)
	switch {
	case streamErr == nil:
		return scan, nil // creds already streamed to the writer
	case ctx.Err() != nil:
		return archiveScan{}, ctx.Err()
	case gate.confirmed && isWrongPassword(streamErr):
		return scan, rarIntegrityError(streamErr)
	case isWrongPassword(streamErr):
		ec.debugf("archive %s: password rejected after %s", ec.display, time.Since(attemptStart).Round(time.Millisecond))
		return scan, streamErr
	default:
		ec.debugf("archive %s: extraction failed after %s: %v",
			ec.display, time.Since(attemptStart).Round(time.Millisecond), streamErr)
		return scan, streamErr
	}
}

// copyMemberIfCandidate copies an env/key archive member flat into the -env
// secrets dir. cr (may be nil) credits the member bytes as they are read;
// pass nil where an outer counting reader or per-member cr.add already covers
// the stream (the RAR paths).
// Returns true when the member stream was fully consumed.
func copyMemberIfCandidate(ctx context.Context, ec extractCtx, cr *creditor, r io.Reader, name string) bool {
	if ec.env == nil || !isEnvCopyCandidate(name) {
		return false
	}
	if !ec.env.CopyMember(ctx, ec.display, name, countingReader{r: r, c: cr}) {
		_, _ = io.Copy(io.Discard, r)
	}
	return true
}

// The caller (extractRarOnce) wraps ec in a gatedSink, so creds emitted here are
// withheld only until ec.confirmPassword() fires at the first proven member
// boundary -- then they stream to the writer, never before the password proves.
func readRarStream(ctx context.Context, ec extractCtx, rr *rardecode.Reader) (archiveScan, error) {
	if ec.depth == 0 && ec.spill == nil {
		ec.spill = newSpillBudget(0, 0)
	}
	ec.stage(StageExtracting)
	tg := newTdataStager(ec.env)
	defer tg.cleanup()
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
				return classifyRarReadError(err, false)
			}
			if h.IsDir {
				continue
			}
			members++
			ec.heartbeat(members)
			// A tdata-prefixed member is staged for whole-tree copy; the stager
			// consumes the member stream. Copy errors (CRC / wrong password)
			// must fail the stream so password retry still runs.
			if consumed, serr := stageIfTdata(tg, h.Name, rr); consumed {
				if ferr := tdataStageStreamErr(ec, serr); ferr != nil {
					return classifyRarReadError(ferr, h.Encrypted)
				}
				continue
			}
			// Force the first member's body through the decoder so a wrong
			// password fails on its first CRC check, instead of (for solid
			// archives) decompressing every member up to the first credential
			// file. Members the switch already reads in full need no separate
			// drain.
			if !validated {
				validated = true
				if !isArchiveFile(h.Name) && !isPasswordFile(h.Name) {
					if copyMemberIfCandidate(ctx, ec, nil, rr, h.Name) {
						continue
					}
					// Drain the rest of the first member so the decoder's CRC
					// runs and a wrong password fails cheaply here.
					if _, derr := io.Copy(io.Discard, rr); derr != nil {
						return classifyRarReadError(derr, h.Encrypted)
					}
					continue
				}
			}
			switch {
			case isArchiveFile(h.Name):
				// Spill the current entry now (before the next Next()); bytes are
				// counted by the outer file reader, so the spill creditor is nil.
				if derr := spillAndDispatch(ctx, ec, &wg, &outcomes, rr, h.Name, nil, h.Solid, h.Encrypted); derr != nil {
					return derr
				}
			case isPasswordFile(h.Name):
				// Emit inline in stream order: pre-confirm the gate buffers these,
				// post-confirm they stream straight to the writer so the live
				// found-counter climbs. Nested archives stay on the outcome path
				// (merged at EOF) since they finish out of order.
				sink := newCredSink(ec.tempDir, ec.display+"!"+h.Name)
				mixed, perr := ec.parse(rr, ec.display+"!"+h.Name, sink.add)
				if perr != nil {
					sink.discard()
					if fatal := classifyRarMemberError(ctx, perr, h.Encrypted); fatal != nil {
						return fatal
					}
					// Isolated content parse failure: record the member and keep
					// going with its siblings, like the zip member loop.
					ec.onIssue(ec.display+"!"+h.Name, IssueParseError, perr)
					if h.Solid {
						// A solid window must stay aligned: drain the rest of the
						// member's decoded bytes before the next Next().
						if _, derr := io.Copy(io.Discard, rr); derr != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							return classifyRarReadError(derr, h.Encrypted)
						}
					}
					continue
				}
				if mixed {
					// Mixed member (review H-20): labeled precedence kept,
					// but the discarded colon lines are real credentials —
					// flag the archive so -del keeps it and history does not
					// record it complete.
					ec.onIssue(ec.display+"!"+h.Name, IssueMixedFormat, nil)
				}
				ec.countCredFile(&scan)
				replaySink(ec, sink)
			default:
				if copyMemberIfCandidate(ctx, ec, nil, rr, h.Name) {
					continue
				}
				// Non-credential member: skip it; rr.Next() skips any
				// unread remainder.
				if _, derr := io.Copy(io.Discard, rr); derr != nil {
					return classifyRarReadError(derr, h.Encrypted)
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
	if ctx.Err() == nil && streamErr == nil {
		promoteTdataOrReport(ec, tg)
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
	candidates := ec.orderedPasswords()
	if len(candidates) == 0 {
		return archiveScan{}, fmt.Errorf("%w: no candidates", errPasswordNotFound)
	}
	scan, err := extractRarVolumesOnce(ctx, ec, first, candidates[0], cr, total, true)
	if err == nil {
		ec.winner.promote(candidates[0])
	}
	if err == nil || ctx.Err() != nil || !isWrongPassword(err) {
		return scan, err
	}
	rest := rarRaceOrder(candidates[1:])
	if len(rest) == 0 {
		return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, err)
	}
	ec.debugf("archive %s: first password wrong, racing %d candidate(s) on first member (multi-volume)", ec.display, len(rest))
	winner, ok, probeErr := raceProbe(ctx, ec, rest, rarVolumeProbe(first))
	if ctx.Err() != nil {
		return archiveScan{}, ctx.Err()
	}
	if probeErr != nil {
		return archiveScan{}, probeErr
	}
	if !ok {
		return archiveScan{}, fmt.Errorf("%w: all %d candidates rejected", errPasswordNotFound, len(candidates))
	}
	scan, err = extractRarVolumesOnce(ctx, ec, first, winner, cr, total, false)
	if err == nil {
		ec.winner.promote(winner)
	}
	return scan, err
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
	rc, err := rardecode.OpenReader(first, rardecode.Password(pw))
	if err != nil {
		return archiveScan{}, err
	}
	// The gate streams creds through once the password is proven; keep its handle
	// so the salvage path can force a flush when a truncation error interrupts
	// the normal boundary-confirm.
	gatedEc, gate := newGatedSink(ec)
	defer rc.Close()
	scan, streamErr := readRarVolumeStream(ctx, gatedEc, rc, cr, total)
	nvol := len(rc.Volumes())
	switch {
	case streamErr == nil:
		return scan, nil // creds already streamed to the writer
	case ctx.Err() != nil:
		return archiveScan{}, ctx.Err()
	case gate.confirmed && isWrongPassword(streamErr):
		return scan, rarIntegrityError(streamErr)
	case isMissingVolume(streamErr):
		// Truncated set: every part on disk decoded cleanly and only a trailing
		// volume is absent. The missing-volume error is what stopped the stream,
		// so the last boundary-confirm may not have fired -- flush explicitly to
		// keep the creds read before the gap, then flag it. Volumes() counts the
		// volume rardecode TRIED to open and failed on, so the parts actually
		// decoded are nvol-1 (clamped to the on-disk total and never negative).
		partsRead := nvol - 1
		if partsRead > total {
			partsRead = total
		}
		if partsRead < 0 {
			partsRead = 0
		}
		gate.confirm()
		ec.onIssue(ec.display, IssueMissingVolume,
			fmt.Errorf("%w: next volume missing after %d part(s)", errIncompleteVolumeSet, partsRead))
		ec.debugf("archive %s: incomplete set, next volume missing after %d/%d part(s); kept creds from parts read",
			ec.display, partsRead, total)
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

// salvageReader wraps a RAR volume-stream reader for credential-member parsing.
// rardecode surfaces a missing continuation volume as a plain *fs.PathError
// read error in the MIDDLE of a member whose decoded data crosses into the
// absent part. The member parser is all-or-nothing (it returns credentials
// only on a clean end-of-member), so without this wrapper every credential
// line decoded before the gap is discarded: a truncated set salvages zero
// lines even though the parts present decoded cleanly, and the "kept creds
// from parts read" note lies. The wrapper converts exactly that error into
// io.EOF for the parser (so the prefix parses and streams) and records the gap
// in st, which the stream loop turns into a missing-volume error routing to
// the salvage path. Any other error passes through untouched.
type salvageReader struct {
	r  io.Reader
	st *salvageState
}

func (s salvageReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && isMissingVolume(err) {
		s.st.missingVolume.Store(true)
		return n, io.EOF
	}
	return n, err
}

// salvageState records that a wrapped read hit a missing continuation volume.
type salvageState struct {
	missingVolume atomic.Bool
}

// readRarVolumeStream mirrors readRarStream for the OpenReader (multi-volume)
// path: it credits progress per member (PackedSize, since OpenReader hides the
// per-volume file reads), advances the "part N/M" worker label, and offloads
// nested-archive processing to the pool the same way.
func readRarVolumeStream(ctx context.Context, ec extractCtx, rc *rardecode.ReadCloser, cr *creditor, total int) (archiveScan, error) {
	if ec.depth == 0 && ec.spill == nil {
		ec.spill = newSpillBudget(0, 0)
	}
	ec.stage(StageExtracting)
	tg := newTdataStager(ec.env)
	defer tg.cleanup()
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
				return classifyRarReadError(err, false)
			}
			if h.IsDir {
				continue
			}
			members++
			ec.heartbeat(members)
			if consumed, serr := stageIfTdata(tg, h.Name, rc); consumed {
				if ferr := tdataStageStreamErr(ec, serr); ferr != nil {
					return classifyRarReadError(ferr, h.Encrypted)
				}
				cr.add(h.PackedSize)
				continue
			}
			// Credit this member's on-disk (packed) bytes once accounted for, so
			// the bar advances per member instead of jumping per ~GB volume;
			// finish() tops the small remainder to 100%.
			if !validated {
				validated = true
				if !isArchiveFile(h.Name) && !isPasswordFile(h.Name) {
					if copyMemberIfCandidate(ctx, ec, nil, rc, h.Name) {
						cr.add(h.PackedSize)
						continue
					}
					// See readRarStream: drain for CRC.
					if _, derr := io.Copy(io.Discard, rc); derr != nil {
						return classifyRarReadError(derr, h.Encrypted)
					}
					cr.add(h.PackedSize)
					continue
				}
			}
			switch {
			case isArchiveFile(h.Name):
				if derr := spillAndDispatch(ctx, ec, &wg, &outcomes, rc, h.Name, nil, h.Solid, h.Encrypted); derr != nil {
					return derr
				}
			case isPasswordFile(h.Name):
				// Emit inline in stream order (see readRarStream); nested archives
				// stay on the outcome path merged at EOF.
				// salvageReader converts a missing-continuation-volume read error
				// (member data crossing into an absent part) into EOF so the
				// lines decoded before the gap still parse and stream — the
				// all-or-nothing parser would otherwise discard them and a
				// truncated set would salvage zero. The decoder cannot continue
				// past the gap (its next Next() fails with an unrelated
				// misalignment), so when the wrapper reports the gap we end the
				// attempt ourselves with a missing-volume error that routes to
				// the salvage branch in extractRarVolumesOnce.
				sv := &salvageState{}
				sink := newCredSink(ec.tempDir, ec.display+"!"+h.Name)
				mixed, perr := ec.parse(salvageReader{r: rc, st: sv}, ec.display+"!"+h.Name, sink.add)
				if perr != nil {
					sink.discard()
					if fatal := classifyRarMemberError(ctx, perr, h.Encrypted); fatal != nil {
						return fatal
					}
					// Isolated content parse failure: record the member and keep
					// going with its siblings, like the zip member loop.
					ec.onIssue(ec.display+"!"+h.Name, IssueParseError, perr)
					if h.Solid {
						// A solid window must stay aligned: drain the rest of the
						// member's decoded bytes before the next Next().
						if _, derr := io.Copy(io.Discard, rc); derr != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							return classifyRarReadError(derr, h.Encrypted)
						}
					}
					continue
				}
				if mixed {
					// Mixed member (review H-20): labeled precedence kept,
					// but the discarded colon lines are real credentials —
					// flag the archive so -del keeps it and history does not
					// record it complete.
					ec.onIssue(ec.display+"!"+h.Name, IssueMixedFormat, nil)
				}
				ec.countCredFile(&scan)
				replaySink(ec, sink)
				cr.add(h.PackedSize)
				if sv.missingVolume.Load() {
					return fmt.Errorf("credential member %s: next volume missing: %w", h.Name, fs.ErrNotExist)
				}
				continue
			default:
				if copyMemberIfCandidate(ctx, ec, nil, rc, h.Name) {
					cr.add(h.PackedSize)
					continue
				}
				// Non-credential member: skip it.
				if _, derr := io.Copy(io.Discard, rc); derr != nil {
					return classifyRarReadError(derr, h.Encrypted)
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
	if ctx.Err() == nil && streamErr == nil {
		promoteTdataOrReport(ec, tg)
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
	if ec.depth == 0 && ec.spill == nil {
		ec.spill = newSpillBudget(0, 0)
	}
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	var lastErr error
	emptyArchive := false
	// Distinct-member statistics across the sweep: an early member may decode
	// (and stream credentials) under one candidate while a later member keeps
	// failing, and the archive still ends up password-not-found. The failure
	// path returns the tally so the summary stays honest about what was
	// scanned and emitted.
	tally := &sevenZipTally{seen: map[string]bool{}}
	var lastMember string
passwordLoop:
	for i, pw := range ec.orderedPasswords() {
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

		// Force a small validation member through the decoder up front so a
		// wrong password fails on its CRC check instead of decoding the whole
		// archive — and run that pass on a DISPOSABLE reader. bodgit/sevenzip
		// (aes7z.calculateKey) builds the KDF input in a bytes.Buffer that
		// aliases the salt slice of the parsed AES coder properties, so the
		// first in-process derivation of a password overwrites the IV bytes
		// that follow in the same backing array. Archives whose AES
		// properties carry an IV (7-Zip 23+ writes a random one; older
		// p7zip-era archives carry none and are immune) then decrypt their
		// first block with a corrupted IV in every folder reader created
		// after that derivation — a correct password still fails CRC. Once
		// the key is cached, derivations write nothing, which is why only
		// the first archive per (password, salt) is affected. Running the
		// derivation here and sweeping on a fresh reader keeps the member
		// loop working: the fresh reader's Password() then expects a
		// key-cache hit, and its properties, re-parsed from disk, are
		// never scribbled by THIS attempt's derivation. The hit is
		// expected, not guaranteed: the per-process key cache is a shared
		// size-10 LRU, so a burst of ≥10 new (password, cycles, salt)
		// derivations mid-sweep (other workers, nested archives) can force
		// a re-derivation on the live sweep reader. That scribbles only
		// the deriving folder's own props slice, and same-folder members
		// re-serve the already-built folder reader from the per-reader
		// pool (forward-seek reuse, no re-parse), so the residual risk is
		// LRU eviction plus a second construction of the deriving folder —
		// narrow in practice, but not impossible.
		validationFile := pickSevenZipValidationFile(zr.File)
		if validationFile != nil {
			validationIdx := -1
			for idx, f := range zr.File {
				if f == validationFile {
					validationIdx = idx
					break
				}
			}
			stopValidationHeartbeat := ec.startValidationHeartbeat(ctx, validationFile.Name)
			vcrc, verr := consumeSevenZipMember(ctx, validationFile, func(r io.Reader) error {
				n, err := io.Copy(io.Discard, io.LimitReader(r, maxParseBuffer+1))
				if err != nil {
					return err
				}
				if n > maxParseBuffer {
					return errParseBufferExceeded
				}
				return nil
			})
			stopValidationHeartbeat()
			closeReader()
			switch {
			case verr == nil:
			case ctx.Err() != nil:
				return archiveScan{}, ctx.Err()
			case isWrongPassword(verr):
				lastErr = verr
				ec.debugf("archive %s: password %d/%d incorrect after %s",
					ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond))
				continue
			case errors.Is(verr, errSevenZipMemberCRC):
				if sevenZipCRCIsStructural(ctx, open, pw, validationIdx, vcrc) {
					ec.debugf("archive %s: member %s checksum mismatch is password-independent; not retrying candidates",
						ec.display, validationFile.Name)
					return archiveScan{}, verr
				}
				lastErr = verr
				ec.debugf("archive %s: password %d/%d incorrect after %s",
					ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond))
				continue
			default:
				return archiveScan{}, verr
			}
		} else {
			// No consumable validation member: force the derivation with a
			// one-touch open of the first real member so the sweep reader's
			// Password() below hits the key cache. The derivation is per
			// (password, cycles, salt), so one touch is enough — provided
			// the open succeeds; if it errors, no derivation happens and
			// the sweep relies on the folder-pool masking described above.
			for _, f := range zr.File {
				if f.FileInfo().IsDir() {
					continue
				}
				if rc, oerr := f.Open(); oerr == nil {
					rc.Close()
				}
				break
			}
			closeReader()
		}
		zr, closeReader, err = open(pw)
		if err != nil {
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
		fail := sevenZipFailure{index: -1}
		scan, hadMembers, streamErr := func() (archiveScan, bool, error) {
			defer closeReader()
			return readSevenZipMembers(ctx, gatedEc, zr, cr, &fail, tally)
		}()
		switch {
		case streamErr == nil:
			if !hadMembers {
				emptyArchive = true
				break passwordLoop
			}
			ec.winner.promote(pw)
			gate.confirm() // flush any tail not yet streamed (e.g. nested-only sets)
			return scan, nil
		case ctx.Err() != nil:
			return archiveScan{}, ctx.Err()
		case isWrongPassword(streamErr):
			if fail.index >= 0 {
				lastMember = fail.name
			}
			lastErr = streamErr
			ec.debugf("archive %s: password %d/%d incorrect after %s",
				ec.display, i+1, len(ec.passwords), time.Since(attemptStart).Round(time.Millisecond))
			continue
		case errors.Is(streamErr, errSevenZipMemberCRC):
			if fail.index >= 0 && sevenZipCRCIsStructural(ctx, open, pw, fail.index, fail.crc) {
				ec.debugf("archive %s: member %s checksum mismatch is password-independent; not retrying candidates",
					ec.display, fail.name)
				return scan, streamErr
			}
			if fail.index >= 0 {
				lastMember = fail.name
			}
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
	if errors.Is(lastErr, errSevenZipMemberCRC) {
		return tally.partial, lastErr
	}
	if lastMember != "" {
		// Name the member that still rejected every candidate so the issue
		// text identifies which folder needs a different password.
		return tally.partial, fmt.Errorf("%w: member %q still rejected after every candidate: %v",
			errPasswordNotFound, lastMember, lastErr)
	}
	return tally.partial, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

func consumeSevenZipMember(ctx context.Context, f *sevenzip.File, consume func(io.Reader) error) (uint32, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	crc := crc32.NewIEEE()
	reader := io.TeeReader(rc, crc)
	consumeErr := consume(contextReader{ctx: ctx, r: reader})
	var drainErr error
	if consumeErr == nil {
		drainErr = drainContext(ctx, reader)
	}
	closeErr := rc.Close()
	if consumeErr != nil {
		return 0, consumeErr
	}
	if drainErr != nil {
		return 0, drainErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	sum := crc.Sum32()
	if f.CRC32 != 0 && sum != f.CRC32 {
		return sum, fmt.Errorf("%w: %s", errSevenZipMemberCRC, f.Name)
	}
	return sum, nil
}

// sevenZipCRCProbePassword never appears in a candidate list; it exists only
// to re-decode a member password-independently for the structural-CRC check
// below.
const sevenZipCRCProbePassword = "\x00sflog-structural-probe\x01"

// sevenZipMemberDigest decodes member idx of the archive `open` yields with pw
// and returns the CRC32 of the full decoded stream. It is used only by the
// structural-CRC probe (one extra decode of a single member, on the failure
// path only).
func sevenZipMemberDigest(ctx context.Context, open func(string) (*sevenzip.Reader, func() error, error), pw string, idx int) (uint32, error) {
	zr, closeReader, err := open(pw)
	if err != nil {
		return 0, err
	}
	defer closeReader()
	if idx < 0 || idx >= len(zr.File) {
		return 0, fmt.Errorf("sflog: member index %d out of range", idx)
	}
	rc, err := zr.File[idx].Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	h := crc32.NewIEEE()
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: rc}); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// sevenZipCRCIsStructural reports whether a member CRC mismatch observed while
// decoding with pw is password-independent. bodgit verifies only header
// digests, so sflog's own member-CRC check is the only content verifier —
// which makes errSevenZipMemberCRC ambiguous: a wrong password on an
// encrypted copy-mode member and genuine corruption both surface as a clean
// decode followed by a CRC mismatch. The discriminator re-decodes the same
// member under the probe password: identical digests mean the password does
// not participate in this member's decode (structural corruption no candidate
// can fix); a failing or differing probe decode means the password matters
// and the sweep must keep trying candidates. The probe runs on a disposable
// reader like every sweep attempt, so the key-derivation cache rules described
// at the validation site apply unchanged.
func sevenZipCRCIsStructural(ctx context.Context, open func(string) (*sevenzip.Reader, func() error, error), pw string, idx int, crc uint32) bool {
	probeCRC, err := sevenZipMemberDigest(ctx, open, sevenZipCRCProbePassword, idx)
	if err != nil {
		// The password changes decode behavior (header-encrypted open, AES
		// auth, decode error): the CRC failure is password-bound.
		return false
	}
	return probeCRC == crc
}

// pickSevenZipValidationFile chooses the up-front validation member. It must
// be the EARLIEST qualifying member in stream order, not the smallest: in a
// solid archive, reading member k means decoding its folder stream from the
// start, so a positionally-late small member turns the "fast" validation into
// a near-full decode (observed as an 8.5-minute silent stall on a real 3.3GB
// solid 7z while a sibling archive validated in under 10s). zr.File is in
// stream order, so the first qualifying member is the cheapest to reach.
// Qualifying: not a directory, non-empty, and decodable within the parse
// buffer cap.
func pickSevenZipValidationFile(files []*sevenzip.File) *sevenzip.File {
	for _, f := range files {
		if f.FileInfo().IsDir() || f.UncompressedSize == 0 ||
			f.UncompressedSize > maxParseBuffer {
			continue
		}
		return f
	}
	return nil
}

// sevenZipFailure describes the member whose failure ended a password
// attempt, so the sweep can (a) probe whether a member-CRC mismatch is
// password-independent and (b) name the member that still needs another
// password. Zero value / index < 0 means no member-level failure is known.
type sevenZipFailure struct {
	name  string
	index int
	crc   uint32
}

// sevenZipTally accumulates the distinct-member scan statistics across a 7z
// password sweep. An early member can decode and stream its credentials under
// one candidate while a later member (a different 7z folder) keeps failing
// verification, and no candidate then decodes BOTH folders — the archive
// fails as password-not-found even though members were scanned and
// credentials emitted. The tally keeps those partial statistics honest and
// dedups per member name so repeated sweep attempts never double-count.
type sevenZipTally struct {
	seen    map[string]bool
	partial archiveScan
}

func (t *sevenZipTally) countCred(name string) {
	if t == nil || t.seen[name] {
		return
	}
	t.seen[name] = true
	t.partial.files++
}

func (t *sevenZipTally) countNested(name string, ns archiveScan) {
	if t == nil || t.seen[name] {
		return
	}
	t.seen[name] = true
	t.partial.nestedArchives += 1 + ns.nestedArchives
	t.partial.files += ns.files
}

func readSevenZipMembers(ctx context.Context, ec extractCtx, zr *sevenzip.Reader, cr *creditor, fail *sevenZipFailure, tally *sevenZipTally) (scan archiveScan, hadMembers bool, err error) {
	ec.stage(StageExtracting)
	tg := newTdataStager(ec.env)
	defer tg.cleanup()
	tdataStageOK := true
	defer func() {
		if ctx.Err() != nil || err != nil || !tdataStageOK {
			return
		}
		promoteTdataOrReport(ec, tg)
	}()
	members := 0
	var uncompressed int64
	var tdataNames []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if isArchiveFile(f.Name) || isPasswordFile(f.Name) {
			uncompressed += f.FileInfo().Size()
		} else if _, _, ok := tdataMemberPrefix(f.Name); ok && ec.env != nil {
			// tdata members are read during the copy tail; keeping them out of
			// the denominator let the cred pass saturate the bar early.
			uncompressed += f.FileInfo().Size()
		} else if isEnvCopyCandidate(f.Name) {
			// Env candidates are copied during the copy tail too.
			uncompressed += f.FileInfo().Size()
		}
		if _, _, ok := tdataMemberPrefix(f.Name); ok {
			tdataNames = append(tdataNames, f.Name)
		}
	}
	tdataOK := tdataConfirmedPrefixes(tdataNames)
	cr.useScale(uncompressed)
	for idx, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		isArch := isArchiveFile(f.Name)
		if !isArch && !isPasswordFile(f.Name) {
			member := f
			if p, _, ok := tdataMemberPrefix(member.Name); ok && ec.env != nil {
				if !tdataOK[p] {
					continue
				}
				rc, oerr := member.Open()
				if oerr != nil {
					if isWrongPassword(oerr) {
						return scan, hadMembers, oerr
					}
					reportTdataIssue(ec, oerr)
					tdataStageOK = false
					continue
				}
				_, serr := stageIfTdata(tg, member.Name, countingReader{r: rc, c: cr})
				rc.Close()
				if serr != nil {
					if isWrongPassword(serr) {
						return scan, hadMembers, serr
					}
					if errors.Is(serr, errTdataOverCap) {
						ec.env.bumpSkippedTdataOverCap()
						reportTdataIssue(ec, serr)
						continue
					}
					reportTdataIssue(ec, serr)
					tdataStageOK = false
					continue
				}
				hadMembers = true
				continue
			}
			if isEnvCopyCandidate(member.Name) {
				rc, oerr := member.Open()
				if oerr == nil {
					if copyMemberIfCandidate(ctx, ec, cr, rc, member.Name) {
						rc.Close()
						continue
					}
					rc.Close()
				}
			}
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
				return scan, hadMembers, rerr
			}
			ns.nestedArchives++
			scan.add(ns)
			tally.countNested(member.Name, ns)
			continue
		}
		var mixed bool
		memberCRC, parseErr := consumeSevenZipMember(ctx, member, func(r io.Reader) error {
			// Credentials stream straight through the gate as they parse: the
			// gate already owns the pre-confirm buffering, so records never
			// accumulate outside it.
			var err error
			mixed, err = ec.parse(countingReader{r: r, c: cr}, ec.display+"!"+member.Name,
				func(c Credential) error { ec.emit(c); return nil })
			return err
		})
		if parseErr != nil {
			if ctx.Err() != nil {
				return scan, hadMembers, ctx.Err()
			}
			// Wrong-password symptoms and member checksum mismatches keep
			// failing the attempt so the password loop advances; structural
			// decoder failures stop re-reading per candidate.
			if isWrongPassword(parseErr) || errors.Is(parseErr, errSevenZipMemberCRC) || isSevenZipDecoderFailure(parseErr) {
				if fail != nil {
					*fail = sevenZipFailure{name: member.Name, index: idx, crc: memberCRC}
				}
				return scan, hadMembers, parseErr
			}
			// Isolated content parse failure: record the member and keep going
			// with its siblings, like the zip member loop. The member stream is
			// already consumed and closed by consumeSevenZipMember.
			ec.onIssue(ec.display+"!"+member.Name, IssueParseError, parseErr)
			continue
		}
		if mixed {
			// Mixed member (review H-20): labeled precedence kept, but the
			// discarded colon lines are real credentials — flag the archive
			// so -del keeps it and history does not record it complete.
			ec.onIssue(ec.display+"!"+member.Name, IssueMixedFormat, nil)
		}
		ec.countCredFile(&scan)
		tally.countCred(member.Name)
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
