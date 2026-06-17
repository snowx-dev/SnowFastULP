// Package selfupdate implements the `update` / `upgrade` CLI subcommand shared
// by sfu and sfs. It queries the latest GitHub release, verifies the matching
// platform asset against the published SHA256SUMS, and atomically swaps the
// installed binaries in place.
//
// Both binaries ship as a pair from the same release, so a single `update`
// refreshes whichever of sfu/sfs live alongside the running executable, keeping
// their versions in lockstep.
//
// The atomic swap (including the Windows "can't overwrite a running .exe"
// rename-aside dance) is delegated to github.com/minio/selfupdate.
package selfupdate

import (
	"crypto"
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
	latestURL = "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/releases/latest"

	// sumsAsset is the checksum manifest published with every release.
	sumsAsset = "SHA256SUMS"

	httpTimeout = 60 * time.Second
)

// product maps an on-disk binary name to its release asset prefix.
type product struct {
	bin    string // executable basename, no extension (e.g. "sfu")
	prefix string // release asset prefix (e.g. "SnowFastULP")
}

// products is the binary pair shipped by each release.
var products = []product{
	{bin: "sfu", prefix: "SnowFastULP"},
	{bin: "sfs", prefix: "SnowFastSearch"},
}

// ghRelease is the subset of the GitHub release API response we consume.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Run executes the update subcommand. args are the tokens following "update".
// currentVersion is the embedded version.String of the running binary. Output
// (progress, results) is written to out. A nil return means success (or already
// up to date); a non-nil error means nothing was changed unless explicitly
// stated in the message.
func Run(args []string, currentVersion string, out io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("update takes no arguments")
	}

	suffix, err := assetSuffix()
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "checking for updates…")
	rel, err := fetchLatest()
	if err != nil {
		return fmt.Errorf("could not reach the release server: %w", err)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	cur := strings.TrimPrefix(currentVersion, "v")
	if latest == "" {
		return fmt.Errorf("latest release has no version tag")
	}
	// compareVersions <= 0 means the latest release is not newer than what's
	// running, so there's nothing to do — and we never silently downgrade.
	if compareVersions(latest, cur) <= 0 {
		fmt.Fprintf(out, "already up to date (%s)\n", cur)
		return nil
	}

	// Resolve the directory holding the running binary; siblings live beside it.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate running executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	dir := filepath.Dir(self)
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	sums, err := fetchSums(rel)
	if err != nil {
		return err
	}

	var updated []string
	for _, p := range products {
		target := filepath.Join(dir, p.bin+ext)
		if _, statErr := os.Stat(target); statErr != nil {
			continue // sibling not installed here — nothing to update
		}

		assetName := fmt.Sprintf("%s-%s-%s", p.prefix, latest, suffix)
		url := findAsset(rel, assetName)
		if url == "" {
			return fmt.Errorf("release %s has no asset %q for this platform", latest, assetName)
		}
		wantHash, ok := sums[assetName]
		if !ok {
			return fmt.Errorf("%s missing checksum for %q", sumsAsset, assetName)
		}
		if err := downloadAndApply(url, target, wantHash); err != nil {
			return fmt.Errorf("updating %s failed: %w", p.bin, err)
		}
		updated = append(updated, p.bin)
	}

	if len(updated) == 0 {
		return fmt.Errorf("found no installed binaries to update in %s", dir)
	}
	fmt.Fprintf(out, "updated %s to %s\n", strings.Join(updated, ", "), latest)
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

// fetchLatest queries the GitHub API for the most recent published release.
// Draft and prerelease entries are excluded by the /latest endpoint.
func fetchLatest() (*ghRelease, error) {
	req, err := http.NewRequest(http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", repoName+"-selfupdate")

	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
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

	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("malformed release response: %w", err)
	}
	return &rel, nil
}

// findAsset returns the download URL for the named release asset, or "".
func findAsset(rel *ghRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// fetchSums downloads and parses the release SHA256SUMS manifest into a map of
// asset name → expected sha256 digest bytes.
func fetchSums(rel *ghRelease) (map[string][]byte, error) {
	url := findAsset(rel, sumsAsset)
	if url == "" {
		return nil, fmt.Errorf("release has no %s manifest", sumsAsset)
	}
	body, err := httpGet(url)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", sumsAsset, err)
	}
	defer body.Close()

	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, err
	}

	sums := parseSums(data)
	if len(sums) == 0 {
		return nil, fmt.Errorf("%s contained no usable entries", sumsAsset)
	}
	return sums, nil
}

// parseSums parses a sha256sum manifest ("<hex>  <name>" per line) into a map
// of asset name → digest bytes. Blank/malformed lines are skipped; a leading
// '*' binary-mode marker on the name is stripped.
func parseSums(data []byte) map[string][]byte {
	sums := make(map[string][]byte)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		digest, derr := hex.DecodeString(fields[0])
		if derr != nil {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		sums[name] = digest
	}
	return sums
}

// downloadAndApply streams the asset at url and atomically replaces target,
// verifying the SHA256 digest before the swap. On failure it reports whether
// the original binary was rolled back.
func downloadAndApply(url, target string, wantHash []byte) error {
	body, err := httpGet(url)
	if err != nil {
		return err
	}
	defer body.Close()

	err = selfupdate.Apply(body, selfupdate.Options{
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

// httpGet performs a GET with a sane timeout and returns the response body for
// the caller to close. Non-2xx statuses are surfaced as errors.
func httpGet(url string) (io.ReadCloser, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", repoName+"-selfupdate")
	resp, err := (&http.Client{Timeout: httpTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	return resp.Body, nil
}
