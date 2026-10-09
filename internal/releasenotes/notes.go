// Package releasenotes fetches and renders the post-update release notes:
// the "What's new" exit message `<tool> update` prints after a successful
// update. The notes live in a per-release file (update-notes-<version>.md)
// addressed by the
// manifest's notes_url + notes_sha256 pair.
//
// Every failure is soft — the update already succeeded when notes render —
// so the whole surface collapses to either a rendered box or one muted
// "release notes unavailable" line, and the exit code never changes.
package releasenotes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

const (
	// repoOwner/repoName build the "full changelog" footer link. Copied from
	// internal/selfupdate's repoOwner/repoName: importing selfupdate here
	// would be an import cycle (selfupdate imports this package for the
	// ShowNotes seam), and the values move with the repository, not with a
	// render call.
	repoOwner = "snowx-dev"
	repoName  = "SnowFastULP"

	// maxNotesSize caps the fetched notes body at 256 KiB. The 0.3.0 notes
	// are ~25 KB, so this is a 10x headroom ceiling that can never stall a
	// terminal for long. Mirrors the LimitReader + 1-byte oversize check of
	// selfupdate's downloadVerified.
	maxNotesSize = 256 << 10

	// unavailableMessage is the single soft-failure line. No reason string:
	// reasons would either leak transport internals or invite support load,
	// and the mirror contract is manual, so a 404 during the mirror window
	// is expected operator state, not an error condition.
	unavailableMessage = "release notes unavailable"

	notesAccept = "text/markdown, text/plain;q=0.8, */*;q=0.1"
)

// notesTimeout is the whole notes-fetch budget, one dedicated http.Client
// timeout. Reusing the 60 s manifest client was rejected: an optional
// garnish on an already-finished update must not be able to add a minute.
// Package-level var (not const) purely so the timeout-budget tests can
// shrink it and prove the notes client really uses it — the same pattern as
// selfupdate's httpTimeout/assetTimeout.
var notesTimeout = 10 * time.Second

func notesClient() *http.Client { return &http.Client{Timeout: notesTimeout} }

// uaTokenRe accepts the build-identifier shapes we ship ("0.2", "0.2-dev",
// "0.3.1+build5"); binNameRe the valid lowercase binary names. Both mirror
// internal/selfupdate's userAgent validation so the notes request carries
// the same "SnowFastULP-selfupdate/<version> (<bin>)" header discipline —
// including selfupdate's "; gotest" marker under a go test binary (inTest is
// duplicated alongside it; importing selfupdate here would cycle).
var (
	uaTokenRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._\-+~]{0,63}$`)
	binNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

var inTest = testing.Testing // package var so tests can pin both UA shapes

func notesUserAgent(bin, version string) string {
	if !binNameRe.MatchString(bin) {
		bin = "unknown"
	}
	if !uaTokenRe.MatchString(version) {
		version = "unknown"
	}
	ua := repoName + "-selfupdate/" + version + " (" + bin
	if inTest() {
		ua += "; gotest"
	}
	return ua + ")"
}

// requireTransportSecurity rejects notes_url values that would fetch the
// notes over plaintext. The value travels inside the (hand-editable)
// manifest, so a hostile manifest must not be able to downgrade notes
// transport to plain http. Plain http is accepted only for loopback hosts so
// local test servers keep working. Duplicated from internal/selfupdate
// rather than imported: selfupdate imports this package for the ShowNotes
// seam, so the import would cycle.
func requireTransportSecurity(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		switch strings.ToLower(u.Hostname()) { // Hostname strips "[::1]" brackets
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return fmt.Errorf("must be fetched over https; plain http is accepted only for localhost/127.0.0.1/[::1] local test servers, got %q", rawURL)
}

// fetchNotes GETs the notes file with the 10 s budget and the 256 KiB cap.
// Non-200 is an error (404 is the common case until the manual mirror step
// lands).
func fetchNotes(url, bin, currentVersion string) ([]byte, error) {
	if err := requireTransportSecurity(url); err != nil {
		return nil, fmt.Errorf("release notes url: %w", err)
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", notesAccept)
	req.Header.Set("User-Agent", notesUserAgent(bin, currentVersion))

	resp, err := notesClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release notes fetch: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNotesSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxNotesSize {
		return nil, fmt.Errorf("release notes exceed %d bytes", maxNotesSize)
	}
	return body, nil
}

// verifyNotesSHA256 enforces the manifest's notes_sha256. Absent → nil
// (render anyway: genmanifest always emits the hash, so absence means a
// hand-edited or old-hosted manifest, and losing a changelog is never worth
// hard-failing a finished update). Present but not a well-formed 32-byte
// hex digest → error: the same parser discipline as the asset hashes, and a
// corrupt integrity field is treated as an integrity failure, not as
// absence.
func verifyNotesSHA256(body []byte, wantHex string) error {
	wantHex = strings.TrimSpace(wantHex)
	if wantHex == "" {
		return nil
	}
	want, err := hex.DecodeString(wantHex)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("update manifest has invalid notes_sha256")
	}
	got := sha256.Sum256(body)
	if !bytes.Equal(got[:], want) {
		return fmt.Errorf("release notes checksum mismatch")
	}
	return nil
}

// sanitize strips ANSI escapes (ansi.Strip) plus every remaining control
// character (C0, DEL, C1) except \n, so a notes file can never inject
// cursor/color escape attacks into the terminal — even a file whose escape
// sequences ansi.Strip could not fully parse leaves only printable runes
// and newlines.
func sanitize(raw []byte) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range ansi.Strip(string(raw)) {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			// control character: drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

const (
	// Width source: term.GetSize on os.Stdout — the writer the box prints
	// to. This is deliberately different from the TUI surfaces, which
	// measure stderr: update output goes to stdout (see §1.3 of the design).
	// Error or non-positive width (non-TTY, redirected) → fallback 80, and
	// everything is capped at 80 — the classic-safe floor; sfu's 86 cap
	// exists for its stat-block layout and buys nothing here.
	notesFallbackWidth = 80
	notesMaxWidth      = 80

	// leftPad indents every notes line (all three tools indent boxes 4:
	// cmd/sfu/tui.go:31, cmd/sfl/tui.go:31, sfs ditto).
	leftPad = 4

	// narrowThreshold: an effective outer width below this renders the
	// borders-off fallback. gradientBox's minWidth=8 floor would produce a
	// degenerate 1-cell inner column; the plain fallback matches how the
	// tools already trade chrome for legibility on narrow widths.
	narrowThreshold = 40
)

func terminalWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return notesFallbackWidth
	}
	if w > notesMaxWidth {
		return notesMaxWidth
	}
	return w
}

// changelogURL is the client-built "full changelog" footer link, the same
// shape as selfupdate's releaseAssetURL.
func changelogURL(version string) string {
	return fmt.Sprintf("https://github.com/%s/%s/blob/v%s/CHANGELOG.md", repoOwner, repoName, version)
}

// ShowNotes fetches and renders the release notes for a finished update.
// Gates on an empty notesURL before any I/O — manifests without notes keep
// the update output byte-identical, zero requests. Never returns an error
// and never panics past its caller: any failure collapses to one muted
// "release notes unavailable" line on out, leaving the exit code and the
// already-printed success lines untouched.
//
// currentVersion labels the User-Agent (the running binary, like all other
// selfupdate traffic); latestVersion drives the box title and the footer
// changelog link.
func ShowNotes(out io.Writer, notesURL, notesSHA256, latestVersion, bin, currentVersion string) {
	if strings.TrimSpace(notesURL) == "" {
		return
	}
	body, err := fetchNotes(notesURL, bin, currentVersion)
	if err == nil {
		err = verifyNotesSHA256(body, notesSHA256)
	}
	if err == nil {
		err = renderSafely(out, sanitize(body), latestVersion, terminalWidth())
	}
	if err != nil {
		fmt.Fprintln(out, unavailableStyle.Render(unavailableMessage))
	}
}

// renderFn indirection lets tests inject a panicking renderer to prove the
// recover path; production assigns it exactly once, here.
var renderFn = Render

// renderSafely isolates parser/renderer panics behind the soft-failure
// path. A panic mid-render may leave partial output on the terminal, but a
// crash after a successful update would be far worse.
func renderSafely(out io.Writer, notes, version string, width int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("release notes render panic: %v", r)
		}
	}()
	return renderFn(out, notes, version, width, notesGradStart, notesGradEnd)
}
