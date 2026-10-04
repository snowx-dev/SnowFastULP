// Package selfupdate implements the `update` / `upgrade` CLI subcommand shared
// by sfu, sfs, and sfl. It queries the SnowFast update manifest, verifies the
// matching platform asset against the manifest SHA256, and atomically swaps the
// installed binaries in place.
//
// The binaries ship from the same release, so a single `update` refreshes
// whichever of sfu/sfs/sfl live alongside the running executable, keeping their
// versions in lockstep.
//
// The atomic swap of an existing binary is a single rename(2) over a
// fsynced, mode-preserved temp file in the target's directory on non-Windows
// platforms, so the target pathname is never absent. On Windows, where a
// running .exe cannot be overwritten and the swap is a rename-aside dance,
// the swap is guarded by a versioned, recoverable
// update journal (journal.go) so a crash between the renames can never leave
// the executable missing: the next updater startup repairs the interrupted
// swap from the journal before planning anything new.
package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"debug/buildinfo"

	"github.com/snowx-dev/SnowFastULP/internal/releasenotes"
	"github.com/snowx-dev/SnowFastULP/internal/version"
)

const (
	repoOwner = "snowx-dev"
	repoName  = "SnowFastULP"
	latestURL = "https://sfu-update.snowx.dev/"

	maxDownloadSize = 64 << 20 // release binaries are ~5 MiB today
)

// httpTimeout and assetTimeout are package-level vars (not consts) purely so
// the timeout-budget tests can shrink them and prove which budget each HTTP
// path really uses; production values are assigned exactly once, here, and
// nothing but the selfupdate tests ever reassigns them.
var (
	httpTimeout  = 60 * time.Second // manifest fetch: JSON is tiny, fail fast
	assetTimeout = 10 * time.Minute // asset bodies up to 64 MiB on slow links
)

// testHooks holds optional overrides used by integration tests in this package.
// Tests set releaseURL to point metadata fetches at an httptest.Server.
type testHooks struct {
	releaseURL     string
	executablePath string
}

func (h *testHooks) releaseEndpoint() string {
	if h != nil && h.releaseURL != "" {
		return h.releaseURL
	}
	return latestURL
}

func (h *testHooks) resolveSelf() (string, error) {
	if h != nil && h.executablePath != "" {
		// Mirror resolveExecutable: production always works on the
		// symlink-resolved path, so tests injecting an alias path get the
		// same semantics.
		if resolved, err := filepath.EvalSymlinks(h.executablePath); err == nil {
			return resolved, nil
		}
		return h.executablePath, nil
	}
	return resolveExecutable()
}

func httpClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// assetClient serves asset downloads: a longer overall budget than the
// manifest fetch, since a 64 MiB payload on a slow link can legitimately
// take minutes. The maxDownloadSize cap already bounds body reads, so the
// generous timeout only guards against a stalled connection.
func assetClient() *http.Client {
	return &http.Client{Timeout: assetTimeout}
}

// uaTokenRe accepts the build-identifier shapes we ship ("0.2", "0.2-dev",
// "0.3.1+build5"): one conservative token with no spaces or header-breaking
// characters, capped at 64 chars.
var uaTokenRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._\-+~]{0,63}$`)

// userAgent renders the User-Agent for all selfupdate HTTP traffic, e.g.
// "SnowFastULP-selfupdate/0.2.0 (sfu)". A bin name that is not a valid
// lowercase token (empty, spaces, "." stem) or a version that is empty or
// not a token falls back to "unknown", so the header is always a single,
// parseable line.
func userAgent(bin, version string) string {
	if !validBinName(bin) {
		bin = "unknown"
	}
	if !uaTokenRe.MatchString(version) {
		version = "unknown"
	}
	return repoName + "-selfupdate/" + version + " (" + bin + ")"
}

// product maps an on-disk binary name to its release asset prefix.
type product struct {
	bin    string // executable basename, no extension (e.g. "sfu")
	prefix string // release asset prefix (e.g. "SnowFastULP")
}

// products is the binary set shipped by each release. Manifest bins are
// unioned onto this list so a partial bins field cannot drop sfu/sfs/sfl.
var products = []product{
	{bin: "sfu", prefix: "SnowFastULP"},
	{bin: "sfs", prefix: "SnowFastSearch"},
	{bin: "sfl", prefix: "SnowFastLog"},
}

var (
	binNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	prefixRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

func validBinName(name string) bool  { return binNameRe.MatchString(name) }
func validPrefix(prefix string) bool { return prefixRe.MatchString(prefix) }

// resolveProducts returns the bin set for this release: the hardcoded trio
// unioned with any valid manifest-declared bins. Old manifests with no bins
// field return the hardcoded list. A non-empty bins list whose every entry
// is invalid is a malformed manifest and returns an error. Duplicate names
// with a different prefix are an error; the same prefix is ignored.
func resolveProducts(m *updateManifest) ([]product, error) {
	out := make([]product, len(products))
	copy(out, products)
	seen := make(map[string]string, len(products)+8)
	for _, p := range products {
		seen[p.bin] = p.prefix
	}
	if m == nil || len(m.Bins) == 0 {
		return out, nil
	}
	valid := 0
	for _, b := range m.Bins {
		name := strings.ToLower(strings.TrimSpace(b.Name))
		prefix := strings.TrimSpace(b.Prefix)
		if !validBinName(name) || !validPrefix(prefix) {
			continue
		}
		valid++
		if old, ok := seen[name]; ok {
			if old != prefix {
				return nil, fmt.Errorf("update manifest: bin %q has conflicting prefixes %q and %q", name, old, prefix)
			}
			continue
		}
		seen[name] = prefix
		out = append(out, product{bin: name, prefix: prefix})
	}
	if valid == 0 {
		return nil, fmt.Errorf("update manifest: bins list has no valid entries")
	}
	return out, nil
}

// updateManifest is the controlled update metadata served from sfu-update.snowx.dev.
type updateManifest struct {
	Version string                   `json:"version"`
	Bins    []manifestBin            `json:"bins,omitempty"`
	Assets  map[string]manifestAsset `json:"assets"`
	// NotesURL/NotesSHA256 point at the per-release notes file
	// (update-notes-<version>.md). Optional: old manifests omit them and ShowNotes gates
	// on the empty URL before any I/O. The lenient decode (no
	// DisallowUnknownFields) means 0.2.x clients silently drop these fields.
	NotesURL    string `json:"notes_url,omitempty"`
	NotesSHA256 string `json:"notes_sha256,omitempty"`
}

type manifestBin struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

type manifestAsset struct {
	SHA256 string `json:"sha256"`
	URL    string `json:"url,omitempty"`
}

type pendingUpdate struct {
	bin    string
	target string
	url    string
	hash   []byte
	isNew  bool // true when the bin is not yet on disk (install, not update)
}

// applyPayloadHook, when non-nil, replaces applyPayload during tests (used both
// to inject apply failures and to record apply order/targets).
var applyPayloadHook func(data []byte, target string, wantHash []byte) error

// beforeStealRemove, when non-nil, runs in acquireUpdateLock right before
// the stale-lock identity re-check, letting tests simulate a rival stealer
// replacing the lock between the read and the steal (see lockIdentity).
var beforeStealRemove func(path string)

// Dispatch runs the update subcommand when args invokes it ("update"/"upgrade").
// args are the CLI tokens after the program name (os.Args[1:]). It returns
// handled=true when the update path ran — the caller should then exit — together
// with the update result; for any other args it returns (false, nil) so the
// caller proceeds normally. sfu, sfs, and sfl share this dispatch.
func Dispatch(args []string, currentVersion string, out io.Writer) (handled bool, err error) {
	if len(args) == 0 || (args[0] != "update" && args[0] != "upgrade") {
		return false, nil
	}
	return true, run(args[1:], currentVersion, productBasename(os.Args[0]), out, nil)
}

func run(args []string, currentVersion, invoked string, out io.Writer, hooks *testHooks) error {
	if invoked == "" {
		invoked = "sfu"
	}
	usage := formatUpdateUsage(invoked)
	var dryRun bool
	for _, a := range args {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--help", "-h", "-help":
			fmt.Fprintln(out, usage)
			return nil
		default:
			return fmt.Errorf("update: unknown argument %q\n%s", a, usage)
		}
	}

	suffix, err := assetSuffix()
	if err != nil {
		return err
	}

	self, err := hooks.resolveSelf()
	if err != nil {
		return err
	}
	dir := filepath.Dir(self)
	invokedBin := productBasename(self)

	// Unrenamed GitHub assets (SnowFastULP-0.3-linux-amd64) fail closed with a
	// rename hint before any network call. A valid short name not in the
	// hardcoded trio (sfx) is allowed through; the post-fetch check against
	// the union list decides whether it is a known tool.
	if !isKnownBin(invokedBin, products) && !validBinName(invokedBin) {
		return checkInvokedBinaryName(self, products, invokedBin)
	}

	fmt.Fprintln(out, "checking for updates…")
	manifest, err := fetchLatest(hooks, invokedBin, currentVersion)
	if err != nil {
		return fmt.Errorf("could not reach the update server: %w", err)
	}

	latest := strings.TrimPrefix(manifest.Version, "v")
	cur := strings.TrimPrefix(currentVersion, "v")
	if latest == "" {
		return fmt.Errorf("update manifest has no version")
	}

	known, err := resolveProducts(manifest)
	if err != nil {
		return err
	}
	if err := checkInvokedBinaryName(self, known, invokedBin); err != nil {
		return err
	}

	// version.Compare <= 0 means the latest release is not newer than what's
	// running, so there's nothing to do — and we never silently downgrade.
	if version.Compare(latest, cur) <= 0 {
		fmt.Fprintf(out, "already up to date (%s)\n", cur)
		return nil
	}

	ext := exeExt()

	// One updater at a time per install dir: the lock covers recovery and
	// target inspection too, because an interrupted update's journal MUST be
	// repaired (or failed loudly) before any new plan is computed from the
	// on-disk binaries. A held lock therefore surfaces before the payload
	// downloads rather than after them.
	releaseLock, err := acquireUpdateLock(dir)
	if err != nil {
		return err
	}
	defer releaseLock()

	// Recover an interrupted update (RR-1.10) before target inspection: a
	// target missing mid-swap is restored from its backup so planUpdates
	// sees a complete install directory. Runs before every update plan,
	// including dry runs.
	if err := recoverInterruptedUpdate(dir); err != nil {
		return err
	}

	pending, orphans, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err != nil {
		return err
	}
	for _, b := range orphans {
		fmt.Fprintf(out, "note: %s is installed here but no longer receives updates (absent from the update manifest)\n", b)
	}
	if len(pending) == 0 {
		return errNoUpdateTargets(dir, known, invokedBin)
	}

	// Narrate the plan up front so a new file is never a surprise. The
	// "installing" line is gated on !dryRun so a dry run never claims an
	// action it won't perform; dry-run relies on the "would install new"
	// lines below instead.
	var updates, installs []string
	for _, u := range pending {
		if u.isNew {
			installs = append(installs, u.bin)
			if !dryRun {
				fmt.Fprintf(out, "new tool available: %s — installing → %s\n", u.bin, u.target)
			}
		} else {
			updates = append(updates, u.bin)
		}
	}
	if dryRun {
		if len(updates) > 0 {
			fmt.Fprintf(out, "would update: %s (to %s)\n", strings.Join(updates, ", "), latest)
		}
		for _, b := range installs {
			fmt.Fprintf(out, "would install new: %s → %s\n", b, filepath.Join(dir, b+ext))
		}
		fmt.Fprintln(out, "no changes made (dry run)")
		return nil
	}

	// Download and verify every payload before swapping anything on disk.
	payloads := make([][]byte, len(pending))
	for i, u := range pending {
		data, derr := downloadVerified(u.url, u.hash, invokedBin, currentVersion)
		if derr != nil {
			return fmt.Errorf("downloading %s failed: %w", u.bin, derr)
		}
		payloads[i] = data
	}

	// Stage every existing-bin payload and publish the recoverable journal
	// before the apply loop's first destructive rename (a no-op on
	// non-Windows platforms, whose existing-bin swap is a single atomic
	// rename with no crash window to journal).
	rj, err := beginReplaceJournal(dir, pending, payloads)
	if err != nil {
		return err
	}

	// Apply siblings first, the invoked binary last — if apply aborts midway,
	// the running executable is still the old build and the user can retry.
	order := applyOrder(pending, invokedBin)
	var done []string
	for _, i := range order {
		u := pending[i]
		if err := applyPayloadFor(payloads[i], u.target, u.hash, u.isNew); err != nil {
			// Repair any journaled state the failure left behind: a live
			// failure and a restart after a crash run the same evidence-based
			// recovery, which leaves either the verified new or the intact old
			// binary and drops the journal, or fails loudly preserving files.
			if rerr := recoverInterruptedUpdate(dir); rerr != nil {
				return fmt.Errorf("%v (additionally, journal repair failed: %v)", err, rerr)
			}
			if len(done) > 0 {
				// A sibling already swapped/installed: the toolkit is now
				// version-skewed. Say so explicitly and point at the safe
				// recovery — re-running finishes the job.
				return skewError(u, err, latest, done, pending, invokedBin)
			}
			verb := "updating"
			if u.isNew {
				verb = "installing new"
			}
			return fmt.Errorf("%s %s failed: %w", verb, u.bin, err)
		}
		done = append(done, u.bin)
	}

	// Every target replaced and verified: drop the journal so the next run
	// treats the directory as clean.
	if err := rj.finish(); err != nil {
		return err
	}

	// Final narration distinguishes "updated X" from "installed new: X".
	var updatedBins, installedBins []string
	for _, u := range pending {
		if u.isNew {
			installedBins = append(installedBins, u.bin)
		} else {
			updatedBins = append(updatedBins, u.bin)
		}
	}
	if len(updatedBins) > 0 {
		fmt.Fprintf(out, "updated %s to %s\n", strings.Join(updatedBins, ", "), latest)
	}
	for _, b := range installedBins {
		fmt.Fprintf(out, "installed new: %s to %s\n", b, latest)
	}
	// Release notes are the exit-message garnish: fetched only after every
	// swap succeeded, soft-failing into one muted line (never fatal, exit
	// stays 0). The empty-URL gate inside ShowNotes runs before any I/O, so
	// manifests without notes keep this output byte-identical; dry-run and
	// up-to-date returns above never reach it.
	releasenotes.ShowNotes(out, manifest.NotesURL, manifest.NotesSHA256, latest, invokedBin, currentVersion)
	return nil
}

// formatUpdateUsage is the help text for the update subcommand, keyed off
// the invoked binary so sfs/sfl don't print "sfu update".
func formatUpdateUsage(invoked string) string {
	if invoked == "" {
		invoked = "sfu"
	}
	invoked = strings.TrimSuffix(strings.ToLower(filepath.Base(invoked)), ".exe")
	return fmt.Sprintf(`usage: %s update [--dry-run]

  --dry-run      print the update plan and exit without touching disk`, invoked)
}

// skewError builds the lockstep-skew error after a partial apply. It covers
// both "still on the old version" (existing bins that didn't get swapped) and
// "new bin not installed" (new bins that didn't get written), so re-running
// is the documented recovery in either case.
func skewError(failed pendingUpdate, err error, latest string, done []string, pending []pendingUpdate, invokedBin string) error {
	doneSet := make(map[string]bool, len(done))
	for _, b := range done {
		doneSet[b] = true
	}
	var doneUpdated, doneInstalled, stillOld, notInstalled []string
	for _, u := range pending {
		if doneSet[u.bin] {
			if u.isNew {
				doneInstalled = append(doneInstalled, u.bin)
			} else {
				doneUpdated = append(doneUpdated, u.bin)
			}
			continue
		}
		if u.isNew {
			notInstalled = append(notInstalled, u.bin)
		} else {
			stillOld = append(stillOld, u.bin)
		}
	}
	verb := "updating"
	if failed.isNew {
		verb = "installing new"
	}
	msg := fmt.Sprintf("%s %s failed: %v\n", verb, failed.bin, err)
	if len(doneUpdated) > 0 {
		msg += fmt.Sprintf("  already updated to %s: %s\n", latest, strings.Join(doneUpdated, ", "))
	}
	if len(doneInstalled) > 0 {
		msg += fmt.Sprintf("  already installed new: %s\n", strings.Join(doneInstalled, ", "))
	}
	if len(stillOld) > 0 {
		msg += fmt.Sprintf("  still on the old version: %s\n", strings.Join(stillOld, ", "))
	}
	if len(notInstalled) > 0 {
		msg += fmt.Sprintf("  new bin not installed: %s\n", strings.Join(notInstalled, ", "))
	}
	msg += fmt.Sprintf("  the binaries are now out of step — re-run `%s update` to finish", invokedBin)
	return errors.New(msg)
}

func resolveExecutable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate running executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// productBasename returns the executable stem (sfu/sfs), stripping a trailing .exe.
func productBasename(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(strings.ToLower(base), ".exe")
}

func isKnownBin(name string, known []product) bool {
	for _, p := range known {
		if p.bin == name {
			return true
		}
	}
	return false
}

// checkInvokedBinaryName rejects release download names so users rename first.
// known is the resolved bin list for this release (manifest bins or the
// hardcoded fallback) so newly-installed manifest-declared binaries like sfx
// can also self-update, not just the original sfu/sfs/sfl trio.
func checkInvokedBinaryName(selfPath string, known []product, invoked string) error {
	name := productBasename(selfPath)
	if isKnownBin(name, known) {
		return nil
	}
	names := make([]string, len(known))
	for i, p := range known {
		names[i] = p.bin
	}
	hint := strings.Join(names, ", ")
	ext := exeExt()
	var renameLines strings.Builder
	for _, p := range known {
		fmt.Fprintf(&renameLines, "    %s-*  → %s%s\n", p.prefix, p.bin, ext)
	}
	if invoked == "" {
		invoked = "sfu"
	}
	return fmt.Errorf(
		"this executable is named %q; self-update only works when the binary is one of: %s\n"+
			"  rename the release download in %s:\n"+
			"%s"+
			"  place the binaries in the same directory, then run: %s update",
		filepath.Base(selfPath), hint,
		filepath.Dir(selfPath), renameLines.String(), invoked)
}

func isCoreBin(name string) bool {
	for _, p := range products {
		if p.bin == name {
			return true
		}
	}
	return false
}

func errNoUpdateTargets(dir string, known []product, invoked string) error {
	names := make([]string, len(known))
	for i, p := range known {
		names[i] = p.bin + exeExt()
	}
	list := strings.Join(names, ", ")
	var renameLines strings.Builder
	for _, p := range known {
		fmt.Fprintf(&renameLines, "    %s-*  → %s%s\n", p.prefix, p.bin, exeExt())
	}
	if invoked == "" {
		invoked = "sfu"
	}
	return fmt.Errorf(
		"found no installed binaries named %s in %s\n"+
			"  release downloads use names like SnowFastULP-<version>-linux-amd64 — rename them as follows in the same folder, then re-run `%s update`:\n"+
			"%s",
		list, dir, invoked, renameLines.String())
}

// planUpdates returns (pending, orphans, error).
// orphans lists installed hardcoded-trio bins that the manifest no longer
// ships. Bins declared by the manifest but absent from the install dir are
// installed next to the running binary; existing files are protected by the
// foreign-file overwrite guard below.
func planUpdates(manifest *updateManifest, latest, suffix, dir, ext string) ([]pendingUpdate, []string, error) {
	known, err := resolveProducts(manifest)
	if err != nil {
		return nil, nil, err
	}
	// Installed hardcoded-trio bins the manifest no longer ships get one
	// orphan notice line in the run summary (see run). The manifest's bins
	// list is the source of truth: resolveProducts unions the hardcoded trio
	// back in, so a trio bin can only be "no longer shipped" when the
	// manifest omits it here. Only product-name matches count — never
	// git/rg lookalikes, which are not in the trio.
	shipped := make(map[string]bool, len(manifest.Bins)+len(products))
	for _, b := range manifest.Bins {
		shipped[strings.ToLower(strings.TrimSpace(b.Name))] = true
	}
	// A trio bin absent from the manifest's bins list is retired from this
	// release: it must be EXCLUDED from the plan entirely, not held to the
	// trio union resolveProducts produces. Demanding its asset would fail
	// the whole update when the release (correctly) no longer publishes it,
	// and updating it anyway when the asset happens to still be shipped
	// would print the orphan note while the same run swaps the binary —
	// both contradict the note's contract. Retired means: stays on the old
	// version, gets no updates until it reappears in a future manifest.
	orphans := make([]string, 0, len(products))

	var pending []pendingUpdate
	for _, p := range known {
		target := filepath.Join(dir, p.bin+ext)
		_, statErr := os.Stat(target)
		exists := false
		switch {
		case statErr == nil:
			exists = true
		case errors.Is(statErr, fs.ErrNotExist):
			// genuinely missing → will be installed fresh
		default:
			return nil, nil, fmt.Errorf("stat %s: %w", target, statErr)
		}
		// Retired trio bin (manifest declares a bins list and omits this
		// one): excluded from the plan; installed copies are reported as
		// orphans. Old manifests with no bins field keep the implicit trio.
		if len(manifest.Bins) > 0 && isCoreBin(p.bin) && !shipped[p.bin] {
			if exists {
				orphans = append(orphans, p.bin)
			}
			continue
		}
		assetName := fmt.Sprintf("%s-%s-%s", p.prefix, latest, suffix)
		// Overwrite guard: never plan a swap onto an existing file that is
		// not a SnowFast binary — a hardcoded-trio name the user reused
		// (unrelated ~/bin/sfs) and manifest-declared extras are equally
		// protected.
		if exists {
			if err := verifyOverwriteTarget(target); err != nil {
				return nil, nil, err
			}
		}

		asset, ok := manifest.Assets[assetName]
		if !ok {
			if !exists {
				// new bin not published for this platform; skip silently so
				// existing bins can still update.
				continue
			}
			return nil, nil, fmt.Errorf("update manifest %s has no asset %q for this platform", latest, assetName)
		}
		wantHash, err := parseManifestHash(asset.SHA256, assetName)
		if err != nil {
			return nil, nil, err
		}
		url := asset.URL
		if url == "" {
			url = releaseAssetURL(latest, assetName)
		}
		// The manifest endpoint itself is hardcoded https; asset url values
		// are manifest-declared and a hand-edited or hostile manifest must
		// not downgrade payload transport to plaintext http.
		if err := requireTransportSecurity(url); err != nil {
			return nil, nil, fmt.Errorf("update manifest %s: asset %q: %w", latest, assetName, err)
		}
		pending = append(pending, pendingUpdate{
			bin:    p.bin,
			target: target,
			url:    url,
			hash:   wantHash,
			isNew:  !exists,
		})
	}
	return pending, orphans, nil
}

// applyOrder returns pending indices with the invoked binary last.
func applyOrder(pending []pendingUpdate, invokedBin string) []int {
	order := make([]int, 0, len(pending))
	var invokedIdx = -1
	for i, u := range pending {
		if u.bin == invokedBin {
			invokedIdx = i
			continue
		}
		order = append(order, i)
	}
	if invokedIdx >= 0 {
		order = append(order, invokedIdx)
	}
	return order
}

func downloadVerified(url string, wantHash []byte, bin, version string) ([]byte, error) {
	body, err := httpGet(url, bin, version)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	data, err := io.ReadAll(io.LimitReader(body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownloadSize {
		return nil, fmt.Errorf("download exceeds %d bytes", maxDownloadSize)
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], wantHash) {
		return nil, fmt.Errorf("checksum mismatch (got %x, want %x)", got, wantHash)
	}
	return data, nil
}

// applyPayloadFor writes a verified payload to target. For an existing bin
// (isNew=false) it delegates to the platform's replaceExistingBin: on Windows
// the journaled rename-aside dance (see journal.go / replace_windows.go),
// elsewhere a single atomic rename over the target. For a new bin
// (isNew=true) there is nothing to swap, so it writes via a temp file in the
// same dir and renames onto the target, then chmods 0755 — mirroring
// install.sh's install_binary. A crash during a new-bin install cannot lose
// data (the target did not exist before), so no journal is written for it.
func applyPayloadFor(data []byte, target string, wantHash []byte, isNew bool) error {
	if applyPayloadHook != nil {
		return applyPayloadHook(data, target, wantHash)
	}
	if err := verifyChecksum(data, wantHash); err != nil {
		return err
	}
	if !isNew {
		return withPermHint(replaceExistingBin(data, target, wantHash))
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".newbin-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write payload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod new binary: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return withPermHint(fmt.Errorf("install new binary: %w", err))
	}
	return nil
}

// withPermHint appends a container/permissions hint to filesystem errors so
// a root-owned or read-only install dir (common in containers) is
// immediately actionable. The original error text is preserved via %w.
func withPermHint(err error) error {
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w — the install directory may be read-only or root-owned; in a container, rebuild the image instead of updating in place", err)
}

// snowfastModulePath is the module identity a target binary must carry
// before the updater will overwrite it.
const snowfastModulePath = "github.com/snowx-dev/SnowFastULP"

// isSnowFastBinary reports whether path is a Go binary built from this
// module. Reading build info is a plain file read and never mutates
// anything. Symlinked targets must be resolved by the caller (the updater
// works on the EvalSymlinks'd self path, and plan targets live in that same
// real directory).
func isSnowFastBinary(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := buildinfo.Read(f)
	if err != nil {
		return false
	}
	// Main.Path is the module that built the binary; Path itself is the main
	// package path (e.g. …/cmd/sfu), which differs per command.
	return info.Main.Path == snowfastModulePath
}

// verifyOverwriteTarget refuses to plan an overwrite of an existing file
// that is not a SnowFast binary: a user's unrelated ~/bin/sfx (or git, or a
// text file a manifest-declared tool happens to collide with) must never be
// clobbered by a later release shipping the same name. Missing files are
// untouched (fresh installs go through a different path). Returns a
// *refuseOverwriteError so callers can present a clear, actionable message.
func verifyOverwriteTarget(target string) error {
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // fresh install; nothing to protect
		}
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if info.IsDir() {
		return &refuseOverwriteError{path: target,
			reason: "a directory occupies the binary's path"}
	}
	if isSnowFastBinary(target) {
		return nil
	}
	return &refuseOverwriteError{path: target,
		reason: "it is not a SnowFast binary"}
}

// refuseOverwriteError marks a planned overwrite of an existing non-SnowFast
// file. The message tells the user exactly how to proceed.
type refuseOverwriteError struct {
	path   string
	reason string
}

func (e *refuseOverwriteError) Error() string {
	return fmt.Sprintf(
		"refusing to overwrite %s — %s; remove or rename it manually if you want %s to take its place",
		e.path, e.reason, filepath.Base(e.path))
}

// verifyChecksum confirms data matches wantHash (sha256). Split out so both
// the new-bin install path and the platform existing-bin replace path share
// verification with the download step.
func verifyChecksum(data, wantHash []byte) error {
	if len(wantHash) == 0 {
		return nil
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], wantHash) {
		return fmt.Errorf("checksum mismatch: expected %x, got %x", wantHash, sum)
	}
	return nil
}

func assetSuffix() (string, error) {
	return assetSuffixFor(runtime.GOOS, runtime.GOARCH)
}

// assetSuffixFor maps a platform pair to the published release asset suffix.
// Returns an error on platforms we don't ship prebuilt binaries for.
func assetSuffixFor(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "linux-amd64", nil
	case "darwin/arm64":
		return "macos-arm64", nil
	case "windows/amd64":
		return "windows-amd64.exe", nil
	case "linux/arm64":
		// The release publishes ONE static linux/arm64 binary, named
		// -android-arm64: it runs both on-device (Termux/proot/adb, where
		// runtime.GOOS is still "linux") and on real linux/arm64 hosts.
		// See the Makefile's ANDROID_GOOS comment.
		return "android-arm64", nil
	default:
		return "", fmt.Errorf(
			"no prebuilt binaries for %s/%s — build from source (make build)",
			goos, goarch)
	}
}

func releaseAssetURL(version, assetName string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", repoOwner, repoName, version, assetName)
}

// isLoopbackHost reports whether host is a loopback host for which plain
// http is tolerated: "localhost", "127.0.0.1", or "::1" (url.URL.Hostname
// strips the brackets from "[::1]", so both spellings arrive bare).
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// requireTransportSecurity rejects manifest-supplied URLs that would fetch
// payloads or notes over plaintext. The manifest endpoint itself is a
// hardcoded https constant and unaffected, but asset `url` values and
// notes_url travel inside the manifest — a hand-edited or hostile manifest
// must not be able to downgrade the transport. Plain http is accepted only
// for loopback hosts so local test servers keep working.
func requireTransportSecurity(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("must be fetched over https; plain http is accepted only for localhost/127.0.0.1/[::1] local test servers, got %q", rawURL)
}

func parseManifestHash(hexDigest, assetName string) ([]byte, error) {
	digest, err := hex.DecodeString(strings.TrimSpace(hexDigest))
	if err != nil {
		return nil, fmt.Errorf("update manifest has invalid sha256 for %q: %w", assetName, err)
	}
	if len(digest) != sha256.Size {
		return nil, fmt.Errorf("update manifest has invalid sha256 length for %q", assetName)
	}
	return digest, nil
}

// fetchLatest queries the controlled SnowFast update manifest.
func fetchLatest(hooks *testHooks, bin, version string) (*updateManifest, error) {
	req, err := http.NewRequest(http.MethodGet, hooks.releaseEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent(bin, version))

	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no published release found (status 404)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	var manifest updateManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("malformed update manifest: %w", err)
	}
	if manifest.Assets == nil {
		manifest.Assets = map[string]manifestAsset{}
	}
	return &manifest, nil
}

// httpGet performs the asset GET with the longer download budget and returns
// the response body for the caller to close. Non-2xx statuses are surfaced
// as errors.
func httpGet(url, bin, version string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent(bin, version))
	resp, err := assetClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	return resp.Body, nil
}
