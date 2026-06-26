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

func isArchiveFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".zip", ".rar", ".7z":
		return true
	default:
		return false
	}
}

// readArchiveCredentials extracts credentials from an archive, resolving a
// single working password before the extraction pass (so the wordlist is tried
// once, not once per member). weight/p drive smooth progress; emit receives one
// credential at a time. Returns the number of credential files scanned.
func readArchiveCredentials(ctx context.Context, path string, passwords []string, weight int64, p *Progress, emit func(Credential)) (int, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".zip":
		return readZipCredentials(ctx, path, passwords, weight, p, emit)
	case ".rar":
		return readRarCredentials(ctx, path, passwords, weight, p, emit)
	case ".7z":
		return readSevenZipCredentials(ctx, path, passwords, weight, p, emit)
	default:
		return 0, nil
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

func readZipCredentials(ctx context.Context, path string, passwords []string, weight int64, p *Progress, emit func(Credential)) (int, error) {
	zr, err := zipenc.OpenReader(path)
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	var members []*zipenc.File
	var probe *zipenc.File
	var uncompressed int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isPasswordFile(f.Name) {
			continue
		}
		members = append(members, f)
		uncompressed += int64(f.UncompressedSize64)
		if probe == nil && f.IsEncrypted() {
			probe = f
		}
	}
	if len(members) == 0 {
		return 0, nil
	}

	// Resolve a single working password against the first encrypted member, then
	// reuse it for all members. yeka/zip handles both WinZip AES and legacy
	// PKWARE ZipCrypto. Fully-unencrypted archives skip resolution.
	pw := ""
	if probe != nil {
		resolved, ok := resolveZipPassword(probe, passwords)
		if !ok {
			return 0, errPasswordNotFound
		}
		pw = resolved
	}

	cr := newCreditor(p, weight, scaleFor(weight, uncompressed))
	defer cr.finish()

	filesScanned := 0
	for _, f := range members {
		if ctx.Err() != nil {
			return filesScanned, ctx.Err()
		}
		if f.IsEncrypted() {
			f.SetPassword(pw)
		}
		rc, err := f.Open()
		if err != nil {
			return filesScanned, err
		}
		creds, parseErr := ParseCredentials(countingReader{r: rc, c: cr}, path+"!"+f.Name)
		closeErr := rc.Close()
		if parseErr != nil || closeErr != nil {
			return filesScanned, firstErr(parseErr, closeErr)
		}
		filesScanned++
		for _, c := range creds {
			emit(c)
		}
	}
	return filesScanned, nil
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

func readRarCredentials(ctx context.Context, path string, passwords []string, weight int64, p *Progress, emit func(Credential)) (int, error) {
	// One creditor for the whole item: it clamps to weight, so retries across
	// passwords never over-credit progress. rar is streaming, so we buffer
	// credentials and only emit after a clean EOF (guards against a wrong
	// password yielding partial garbage).
	cr := newCreditor(p, weight, 1)
	defer cr.finish()

	var lastErr error
	for _, pw := range passwords {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		f, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		unreg := registerAbort(ctx, f)
		rr, err := rardecode.NewReader(countingReader{r: f, c: cr}, pw)
		if err != nil {
			unreg()
			_ = f.Close()
			lastErr = err
			continue
		}
		creds, filesScanned, streamErr := readRarStream(ctx, path, rr)
		unreg()
		_ = f.Close()
		if streamErr == nil {
			for _, c := range creds {
				emit(c)
			}
			return filesScanned, nil
		}
		lastErr = streamErr
	}
	if lastErr == nil {
		lastErr = errPasswordNotFound
	}
	return 0, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

func readRarStream(ctx context.Context, path string, rr *rardecode.Reader) ([]Credential, int, error) {
	var out []Credential
	filesScanned := 0
	for {
		if ctx.Err() != nil {
			return nil, filesScanned, ctx.Err()
		}
		h, err := rr.Next()
		if errors.Is(err, io.EOF) {
			return out, filesScanned, nil
		}
		if err != nil {
			return nil, filesScanned, err
		}
		if h.IsDir || !isPasswordFile(h.Name) {
			continue
		}
		filesScanned++
		creds, err := ParseCredentials(rr, path+"!"+h.Name)
		if err != nil {
			return nil, filesScanned, err
		}
		out = append(out, creds...)
	}
}

func readSevenZipCredentials(ctx context.Context, path string, passwords []string, weight int64, p *Progress, emit func(Credential)) (int, error) {
	// One creditor for the item so password retries never over-credit. Each
	// candidate is tried in a single pass; credentials are buffered and only
	// emitted after every member decrypts and parses cleanly, so a wrong
	// password (which fails mid-read) yields no partial/garbage output.
	cr := newCreditor(p, weight, 1)
	defer cr.finish()

	var lastErr error
	emptyArchive := false
	for _, pw := range passwords {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		zr, err := sevenzip.OpenReaderWithPassword(path, pw)
		if err != nil {
			lastErr = err // header-encrypted wrong password fails here
			continue
		}
		creds, filesScanned, hadMembers, streamErr := readSevenZipMembers(ctx, path, zr)
		_ = zr.Close()
		if streamErr == nil {
			if !hadMembers {
				emptyArchive = true
				break
			}
			for _, c := range creds {
				emit(c)
			}
			return filesScanned, nil
		}
		lastErr = streamErr
	}
	if emptyArchive {
		return 0, nil
	}
	if lastErr == nil {
		lastErr = errPasswordNotFound
	}
	return 0, fmt.Errorf("%w: %v", errPasswordNotFound, lastErr)
}

func readSevenZipMembers(ctx context.Context, path string, zr *sevenzip.ReadCloser) ([]Credential, int, bool, error) {
	var out []Credential
	filesScanned := 0
	hadMembers := false
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isPasswordFile(f.Name) {
			continue
		}
		hadMembers = true
		if ctx.Err() != nil {
			return nil, filesScanned, hadMembers, ctx.Err()
		}
		rc, err := f.Open()
		if err != nil {
			return nil, filesScanned, hadMembers, err
		}
		creds, parseErr := ParseCredentials(rc, path+"!"+f.Name)
		closeErr := rc.Close()
		if parseErr != nil || closeErr != nil {
			return nil, filesScanned, hadMembers, firstErr(parseErr, closeErr)
		}
		filesScanned++
		out = append(out, creds...)
	}
	return out, filesScanned, hadMembers, nil
}
