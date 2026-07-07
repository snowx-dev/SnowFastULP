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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// products is the binary set shipped by each release.
var products = []product{
	{bin: "sfu", prefix: "SnowFastULP"},
	{bin: "sfs", prefix: "SnowFastSearch"},
	{bin: "sfl", prefix: "SnowFastLog"},
}

// updateManifest is the controlled update metadata served from sfu-update.snowx.dev.
type updateManifest struct {
	Version string                   `json:"version"`
	Assets  map[string]manifestAsset `json:"assets"`
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
	return run(args, currentVersion, out, nil)
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
	return true, Run(args[1:], currentVersion, out)
}

func run(args []string, currentVersion string, out io.Writer, hooks *testHooks) error {
	if len(args) > 0 {
		return fmt.Errorf("update takes no arguments")
	}

	suffix, err := assetSuffix()
	if err != nil {
		return err
	}

	self, err := hooks.resolveSelf()
	if err != nil {
		return err
	}
	if err := checkInvokedBinaryName(self); err != nil {
		return err
	}
	dir := filepath.Dir(self)
	invokedBin := productBasename(self)

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
	// compareVersions <= 0 means the latest release is not newer than what's
	// running, so there's nothing to do — and we never silently downgrade.
	if compareVersions(latest, cur) <= 0 {
		fmt.Fprintf(out, "already up to date (%s)\n", cur)
		return nil
	}

	ext := exeExt()

	pending, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return errNoUpdateTargets(dir)
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
		if err := applyPayload(payloads[i], u.target, u.hash); err != nil {
			if len(done) > 0 {
				// A sibling already swapped: the pair is now version-skewed.
				// Say so explicitly (the binaries are meant to move in lockstep)
				// and point at the safe recovery — re-running finishes the job.
				return fmt.Errorf(
					"updating %s failed: %w\n"+
						"  already updated to %s: %s\n"+
						"  still on the old version: %s\n"+
						"  the binaries are now out of step — re-run `%s update` to finish",
					u.bin, err, latest, strings.Join(done, ", "),
					strings.Join(notUpdated(pending, done), ", "), invokedBin)
			}
			return fmt.Errorf("updating %s failed: %w", u.bin, err)
		}
		done = append(done, u.bin)
	}

	updated := make([]string, len(pending))
	for i, u := range pending {
		updated[i] = u.bin
	}
	fmt.Fprintf(out, "updated %s to %s\n", strings.Join(updated, ", "), latest)
	return nil
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

func isKnownBin(name string) bool {
	for _, p := range products {
		if p.bin == name {
			return true
		}
	}
	return false
}

// checkInvokedBinaryName rejects release download names so users rename first.
func checkInvokedBinaryName(selfPath string) error {
	name := productBasename(selfPath)
	if isKnownBin(name) {
		return nil
	}
	return fmt.Errorf(
		"this executable is named %q; self-update only works when the binary is named %q, %q, or %q\n"+
			"  rename the release download in %s:\n"+
			"    SnowFastULP-*  → sfu%s\n"+
			"    SnowFastSearch-* → sfs%s\n"+
			"    SnowFastLog-* → sfl%s\n"+
			"  place sfu, sfs, and sfl in the same directory, then run: sfu update",
		filepath.Base(selfPath), products[0].bin, products[1].bin, products[2].bin,
		filepath.Dir(selfPath), exeExt(), exeExt(), exeExt())
}

func errNoUpdateTargets(dir string) error {
	return fmt.Errorf(
		"found no installed binaries named sfu%s, sfs%s, or sfl%s in %s\n"+
			"  release downloads use names like SnowFastULP-<version>-linux-amd64 — rename them to sfu%s, sfs%s, and sfl%s in the same folder, then re-run update",
		exeExt(), exeExt(), exeExt(), dir, exeExt(), exeExt(), exeExt())
}

func planUpdates(manifest *updateManifest, latest, suffix, dir, ext string) ([]pendingUpdate, error) {
	var pending []pendingUpdate
	for _, p := range products {
		target := filepath.Join(dir, p.bin+ext)
		if _, statErr := os.Stat(target); statErr != nil {
			continue
		}

		assetName := fmt.Sprintf("%s-%s-%s", p.prefix, latest, suffix)
		asset, ok := manifest.Assets[assetName]
		if !ok {
			return nil, fmt.Errorf("update manifest %s has no asset %q for this platform", latest, assetName)
		}
		wantHash, err := parseManifestHash(asset.SHA256, assetName)
		if err != nil {
			return nil, err
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
		})
	}
	return pending, nil
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

// notUpdated returns the pending binaries not present in done, preserving
// pending order — i.e. the ones still on the old version after a partial apply.
func notUpdated(pending []pendingUpdate, done []string) []string {
	doneSet := make(map[string]bool, len(done))
	for _, b := range done {
		doneSet[b] = true
	}
	var rest []string
	for _, u := range pending {
		if !doneSet[u.bin] {
			rest = append(rest, u.bin)
		}
	}
	return rest
}

func downloadVerified(url string, wantHash []byte, hooks *testHooks) ([]byte, error) {
	body, err := httpGet(url, hooks)
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

func applyPayload(data []byte, target string, wantHash []byte) error {
	if applyPayloadHook != nil {
		return applyPayloadHook(data, target, wantHash)
	}
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
func httpGet(url string, hooks *testHooks) (io.ReadCloser, error) {
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
