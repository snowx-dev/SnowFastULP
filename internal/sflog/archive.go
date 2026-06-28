package sflog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

func isArchiveFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".zip", ".rar", ".7z":
		return true
	default:
		return false
	}
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
}

// stage publishes s to the worker slot if a stage sink is wired (no-op for
// direct/hermetic callers that don't drive the TUI).
func (ec extractCtx) stage(s WorkerStage) {
	if ec.setStage != nil {
		ec.setStage(s)
	}
}

// pendingIssue lets the streaming (rar/7z) readers buffer nested issues and
// only commit them once a password pass succeeds.
type pendingIssue struct {
	path string
	kind IssueKind
	err  error
}

// readArchiveCredentials extracts credentials from an archive at diskPath,
// recursing into nested archive members. weight drives smooth progress; ec.emit
// receives one credential at a time. It returns the files/nested-archive counts.
func readArchiveCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	switch strings.ToLower(filepath.Ext(diskPath)) {
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
	// Nested progress rides on the spill credit above; weight 0 keeps the
	// nested reader's own creditor from double-counting.
	scan, err := readArchiveCredentials(ctx, tmp, child, 0)
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

func readZipCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	zr, err := zipenc.OpenReader(diskPath)
	if err != nil {
		return archiveScan{}, err
	}
	defer zr.Close()

	var credFiles, nestedFiles []*zipenc.File
	var probe *zipenc.File
	var uncompressed int64
	for _, f := range zr.File {
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
		if probe == nil && f.IsEncrypted() {
			probe = f
		}
	}
	if len(credFiles) == 0 && len(nestedFiles) == 0 {
		return archiveScan{}, nil
	}

	// Resolve a single working password against the first encrypted member, then
	// reuse it for all members. yeka/zip handles WinZip AES and legacy ZipCrypto.
	pw := ""
	if probe != nil {
		ec.stage(StageTestingPassword)
		resolved, ok := resolveZipPassword(probe, ec.passwords)
		if !ok {
			return archiveScan{}, errPasswordNotFound
		}
		pw = resolved
	}
	ec.stage(StageExtracting)

	cr := newCreditor(ec.p, weight, scaleFor(weight, uncompressed))
	defer cr.finish()

	var scan archiveScan
	for _, f := range credFiles {
		if ctx.Err() != nil {
			return scan, ctx.Err()
		}
		if f.IsEncrypted() {
			f.SetPassword(pw)
		}
		rc, err := f.Open()
		if err != nil {
			// Member-level isolation: a single unreadable member never discards
			// the rest of the archive.
			ec.onIssue(ec.display+"!"+f.Name, IssueOpenError, err)
			continue
		}
		creds, parseErr := ParseCredentials(countingReader{r: rc, c: cr}, ec.display+"!"+f.Name)
		closeErr := rc.Close()
		if parseErr != nil || closeErr != nil {
			ec.onIssue(ec.display+"!"+f.Name, IssueParseError, firstErr(parseErr, closeErr))
			continue
		}
		scan.files++
		for _, c := range creds {
			ec.emit(c)
		}
	}
	for _, f := range nestedFiles {
		if ctx.Err() != nil {
			return scan, ctx.Err()
		}
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
	for _, pw := range ec.passwords {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.stage(StageTestingPassword)
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
	}
	if lastErr == nil {
		lastErr = errPasswordNotFound
	}
	return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

func readRarStream(ctx context.Context, ec extractCtx, rr *rardecode.Reader) (archiveScan, error) {
	ec.stage(StageExtracting)
	var scan archiveScan
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

func readSevenZipCredentials(ctx context.Context, diskPath string, ec extractCtx, weight int64) (archiveScan, error) {
	// One creditor for the item so password retries never over-credit. Each
	// candidate is tried in a single pass; credentials/issues are buffered and
	// only committed after every member decrypts and parses cleanly, so a wrong
	// password (which fails mid-read) yields no partial/garbage output.
	cr := newCreditor(ec.p, weight, 1)
	defer cr.finish()

	var lastErr error
	emptyArchive := false
	for _, pw := range ec.passwords {
		if ctx.Err() != nil {
			return archiveScan{}, ctx.Err()
		}
		ec.stage(StageTestingPassword)
		zr, err := sevenzip.OpenReaderWithPassword(diskPath, pw)
		if err != nil {
			lastErr = err // header-encrypted wrong password fails here
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
		_ = zr.Close()
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
	}
	if emptyArchive {
		return archiveScan{}, nil
	}
	if lastErr == nil {
		lastErr = errPasswordNotFound
	}
	return archiveScan{}, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

func readSevenZipMembers(ctx context.Context, ec extractCtx, zr *sevenzip.ReadCloser, cr *creditor) (archiveScan, bool, error) {
	ec.stage(StageExtracting)
	var scan archiveScan
	hadMembers := false
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		isArch := isArchiveFile(f.Name)
		if !isArch && !isPasswordFile(f.Name) {
			continue
		}
		hadMembers = true
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
