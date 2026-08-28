// Package selfupdate implements the `update` / `upgrade` CLI subcommand shared
// by sfu, sfs, and sfl. It queries the SnowFast update manifest, verifies the
// matching platform asset against the manifest SHA256, and atomically swaps the
// installed binaries in place.
//
// The binaries ship from the same release, so a single `update` refreshes
// whichever of sfu/sfs/sfl live alongside the running executable, keeping their
// versions in lockstep.
//
// The atomic swap (including the Windows "can't overwrite a running .exe"
// rename-aside dance) is delegated to github.com/minio/selfupdate.
package selfupdate

import (
	"bytes"
	"crypto"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/minio/selfupdate"
)

const (
	repoOwner = "snowx-dev"
	repoName  = "SnowFastULP"
	latestURL = "https://sfu-update.snowx.dev/"

	httpTimeout     = 60 * time.Second
	maxDownloadSize = 64 << 20 // release binaries are ~5 MiB today
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
		return h.executablePath, nil
	}
	return resolveExecutable()
}

func httpClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
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

// Run executes the update subcommand. args are the tokens following "update".
// currentVersion is the embedded version.String of the running binary. Output
// (progress, results) is written to out. A nil return means success (or already
// up to date); a non-nil error means nothing was changed unless explicitly
// stated in the message.
func Run(args []string, currentVersion string, out io.Writer) error {
	return run(args, currentVersion, productBasename(os.Args[0]), out, nil)
}

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
	var (
		installNew bool
		dryRun     bool
	)
	for _, a := range args {
		switch a {
		case "--install-new":
			installNew = true
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
	manifest, err := fetchLatest(hooks)
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

	// compareVersions <= 0 means the latest release is not newer than what's
	// running, so there's nothing to do — and we never silently downgrade.
	if compareVersions(latest, cur) <= 0 {
		fmt.Fprintf(out, "already up to date (%s)\n", cur)
		return nil
	}

	ext := exeExt()
	allowNew := canInstallNewBinsFor(self, installNew)

	pending, skippedNew, err := planUpdates(manifest, latest, suffix, dir, ext, allowNew)
	if err != nil {
		return err
	}
	if len(skippedNew) > 0 {
		fmt.Fprintf(out, "skipped extra tools (%s): install stamp missing or dir mismatch; use `%s update --install-new`\n",
			strings.Join(skippedNew, ", "), invokedBin)
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
		data, derr := downloadVerified(u.url, u.hash, hooks)
		if derr != nil {
			return fmt.Errorf("downloading %s failed: %w", u.bin, derr)
		}
		payloads[i] = data
	}

	// Apply siblings first, the invoked binary last — if apply aborts midway,
	// the running executable is still the old build and the user can retry.
	order := applyOrder(pending, invokedBin)
	var done []string
	for _, i := range order {
		u := pending[i]
		if err := applyPayloadFor(payloads[i], u.target, u.hash, u.isNew); err != nil {
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
	return nil
}

// formatUpdateUsage is the help text for the update subcommand, keyed off
// the invoked binary so sfs/sfl don't print "sfu update".
func formatUpdateUsage(invoked string) string {
	if invoked == "" {
		invoked = "sfu"
	}
	invoked = strings.TrimSuffix(strings.ToLower(filepath.Base(invoked)), ".exe")
	return fmt.Sprintf(`usage: %s update [--dry-run] [--install-new]

  --dry-run      print the update plan and exit without touching disk
  --install-new  allow installing new binaries even when the install marker
                 is absent (escape hatch; the install scripts set the marker
                 for you)`, invoked)
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

func planUpdates(manifest *updateManifest, latest, suffix, dir, ext string, allowNew bool) ([]pendingUpdate, []string, error) {
	known, err := resolveProducts(manifest)
	if err != nil {
		return nil, nil, err
	}
	var pending []pendingUpdate
	var skipped []string
	for _, p := range known {
		target := filepath.Join(dir, p.bin+ext)
		_, statErr := os.Stat(target)
		exists := false
		switch {
		case statErr == nil:
			exists = true
		case errors.Is(statErr, fs.ErrNotExist):
			// genuinely missing → may install if allowed
		default:
			return nil, nil, fmt.Errorf("stat %s: %w", target, statErr)
		}
		// Extra (non-trio) names always need stamp/--install-new, even when
		// the file already exists — otherwise a manifest can overwrite git/rg
		// in a shared bin dir. Missing core bins still need allowNew to install.
		extra := !isCoreBin(p.bin)
		if extra || !exists {
			if !allowNew {
				if extra {
					skipped = append(skipped, p.bin)
				}
				continue
			}
		}

		assetName := fmt.Sprintf("%s-%s-%s", p.prefix, latest, suffix)
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
		pending = append(pending, pendingUpdate{
			bin:    p.bin,
			target: target,
			url:    url,
			hash:   wantHash,
			isNew:  !exists,
		})
	}
	return pending, skipped, nil
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

func downloadVerified(url string, wantHash []byte, hooks *testHooks) ([]byte, error) {
	body, err := httpGet(url)
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
// (isNew=false) it uses selfupdate.Apply, which atomically swaps the file
// with rollback on failure. For a new bin (isNew=true) there is nothing to
// swap, so it writes via a temp file in the same dir and renames onto the
// target, then chmods 0755 — mirroring install.sh's install_binary.
func applyPayloadFor(data []byte, target string, wantHash []byte, isNew bool) error {
	if applyPayloadHook != nil {
		return applyPayloadHook(data, target, wantHash)
	}
	if !isNew {
		err := selfupdate.Apply(bytes.NewReader(data), selfupdate.Options{
			TargetPath: target,
			Checksum:   wantHash,
			Hash:       crypto.SHA256,
		})
		if err != nil {
			if rb := selfupdate.RollbackError(err); rb != nil {
				return fmt.Errorf("%w (ROLLBACK ALSO FAILED: %v — restore %s manually)", err, rb, target)
			}
			return err
		}
		return nil
	}

	if err := verifyChecksum(data, wantHash); err != nil {
		return err
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
		return fmt.Errorf("install new binary: %w", err)
	}
	return nil
}

// verifyChecksum confirms data matches wantHash (sha256). Split out so the
// new-bin install path shares verification with the selfupdate.Apply path.
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

// compareVersions returns -1, 0, or 1 if a is older, equal, or newer than b.
// Versions are dotted-numeric (e.g. "0.1.1"); missing trailing components count
// as 0, so "0.1" == "0.1.0". A prerelease suffix after '-' (e.g. "0.1.1-dev")
// ranks below the same base release, matching semver precedence. Non-numeric
// components are compared by string as a last resort.
func compareVersions(a, b string) int {
	baseA, preA, _ := strings.Cut(a, "-")
	baseB, preB, _ := strings.Cut(b, "-")

	pa := strings.Split(baseA, ".")
	pb := strings.Split(baseB, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		na, sa := numAt(pa, i)
		nb, sb := numAt(pb, i)
		if sa != "" || sb != "" { // fall back to string compare for this field
			if sa != sb {
				return strings.Compare(sa, sb)
			}
			continue
		}
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}

	// Equal base: a release (no prerelease) outranks a prerelease.
	switch {
	case preA == "" && preB == "":
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	default:
		return strings.Compare(preA, preB)
	}
}

// numAt parses the i-th dotted component as an int. If the component is absent
// it's 0; if it's non-numeric, the raw string is returned for fallback compare.
func numAt(parts []string, i int) (int, string) {
	if i >= len(parts) {
		return 0, ""
	}
	n := 0
	for _, r := range parts[i] {
		if r < '0' || r > '9' {
			return 0, parts[i]
		}
		n = n*10 + int(r-'0')
	}
	return n, ""
}

// assetSuffix maps the running platform to the published release asset suffix.
// Returns an error on platforms we don't ship prebuilt binaries for.
func assetSuffix() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "linux-amd64", nil
	case "darwin/arm64":
		return "macos-arm64", nil
	case "windows/amd64":
		return "windows-amd64.exe", nil
	default:
		return "", fmt.Errorf(
			"no prebuilt binaries for %s/%s — build from source (make build)",
			runtime.GOOS, runtime.GOARCH)
	}
}

func releaseAssetURL(version, assetName string) string {
	return fmt.Sprintf("https://github.com/%s/%s/releases/download/v%s/%s", repoOwner, repoName, version, assetName)
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
func fetchLatest(hooks *testHooks) (*updateManifest, error) {
	req, err := http.NewRequest(http.MethodGet, hooks.releaseEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", repoName+"-selfupdate")

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

// httpGet performs a GET with a sane timeout and returns the response body for
// the caller to close. Non-2xx statuses are surfaced as errors.
func httpGet(url string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", repoName+"-selfupdate")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	return resp.Body, nil
}
