package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// buildFixtureSums hashes the given file contents into a SHA256SUMS-shaped
// map for the release assets of version, using the same sha256 algorithm the
// release pipeline's sha256sum applies.
func buildFixtureSums(t *testing.T, version string, contents map[string]string) map[string]string {
	t.Helper()
	sums := make(map[string]string, len(contents))
	for name, data := range contents {
		sum := sha256.Sum256([]byte(data))
		sums[name] = hex.EncodeToString(sum[:])
	}
	return sums
}

func fixtureReleaseAssets(version string) map[string]string {
	binaries := map[string]string{
		"SnowFastULP-" + version + "-linux-amd64":          "sfu linux",
		"SnowFastULP-" + version + "-macos-arm64":          "sfu macos",
		"SnowFastULP-" + version + "-windows-amd64.exe":    "sfu windows",
		"SnowFastSearch-" + version + "-linux-amd64":       "sfs linux",
		"SnowFastSearch-" + version + "-macos-arm64":       "sfs macos",
		"SnowFastSearch-" + version + "-windows-amd64.exe": "sfs windows",
		"SnowFastLog-" + version + "-linux-amd64":          "sfl linux",
		"SnowFastLog-" + version + "-macos-arm64":          "sfl macos",
		"SnowFastLog-" + version + "-windows-amd64.exe":    "sfl windows",
		"SnowFastULP-" + version + "-android-arm64":        "sfu android",
		"SnowFastSearch-" + version + "-android-arm64":     "sfs android",
		"SnowFastLog-" + version + "-android-arm64":        "sfl android",
		"SnowFastULP-" + version + "-binaries.zip":         "zip",
	}
	return binaries
}

// TestBuildUpdateManifestRoundTripsThroughFetcher feeds generator output
// through the real decode path (fetchLatest's JSON decode) and the planning
// lookups (resolveProducts, asset lookup, parseManifestHash), asserting the
// expected version, bins, and checksums survive.
func TestBuildUpdateManifestRoundTripsThroughFetcher(t *testing.T) {
	const version = "0.3.1"
	assets := fixtureReleaseAssets(version)
	sums := buildFixtureSums(t, version, assets)

	out, err := BuildUpdateManifest(version, sums)
	if err != nil {
		t.Fatalf("BuildUpdateManifest: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	manifest, err := fetchLatest(&testHooks{releaseURL: srv.URL}, "sfu", "0.2")
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if manifest.Version != version {
		t.Fatalf("decoded version = %q, want %q", manifest.Version, version)
	}

	known, err := resolveProducts(manifest)
	if err != nil {
		t.Fatalf("resolveProducts: %v", err)
	}
	if len(known) != len(products) {
		t.Fatalf("resolveProducts returned %d products, want %d", len(known), len(products))
	}
	for i, p := range products {
		if known[i] != p {
			t.Fatalf("resolveProducts[%d] = %+v, want %+v", i, known[i], p)
		}
	}
	if len(manifest.Bins) != len(products) {
		t.Fatalf("manifest bins = %d entries, want %d", len(manifest.Bins), len(products))
	}

	for assetName := range assets {
		asset, ok := manifest.Assets[assetName]
		if !ok {
			t.Fatalf("decoded manifest has no asset %q", assetName)
		}
		want := sums[assetName]
		got, err := parseManifestHash(asset.SHA256, assetName)
		if err != nil {
			t.Fatalf("parseManifestHash(%q): %v", assetName, err)
		}
		if hex.EncodeToString(got) != want {
			t.Fatalf("asset %q digest = %s, want %s", assetName, hex.EncodeToString(got), want)
		}
	}

	// The windows asset key embeds ".exe" the way assetSuffix reports it —
	// pin the exact shape planUpdates will look up.
	for _, p := range products {
		key := fmt.Sprintf("%s-%s-windows-amd64.exe", p.prefix, version)
		if _, ok := manifest.Assets[key]; !ok {
			t.Fatalf("manifest missing windows asset key %q", key)
		}
	}
}

// TestBuildUpdateManifestDigestsMatchFilesOnDisk pins the single-source-of-
// truth contract end to end: files are written to disk, a SHA256SUMS artifact
// is built from those bytes (the sha256 algorithm the release pipeline's
// sha256sum applies), parsed back with ParseSHA256SUMS, and the resulting
// manifest digests must equal digests recomputed independently over the
// on-disk files.
func TestBuildUpdateManifestDigestsMatchFilesOnDisk(t *testing.T) {
	const version = "0.9.0"
	dir := t.TempDir()
	assets := fixtureReleaseAssets(version)

	var sumsLines []string
	for name, data := range assets {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(data))
		sumsLines = append(sumsLines, hex.EncodeToString(sum[:])+"  "+name)
	}
	sums, err := ParseSHA256SUMS(strings.NewReader(strings.Join(sumsLines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("ParseSHA256SUMS: %v", err)
	}

	out, err := BuildUpdateManifest(version, sums)
	if err != nil {
		t.Fatalf("BuildUpdateManifest: %v", err)
	}

	// Decode the emitted JSON independently of the internal structs.
	var wire struct {
		Version string `json:"version"`
		Bins    []struct {
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"bins"`
		Assets map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("generated manifest is not valid JSON: %v\n%s", err, out)
	}
	if wire.Version != version || len(wire.Bins) != 3 || len(wire.Assets) != len(assets) {
		t.Fatalf("unexpected wire shape: version=%q bins=%d assets=%d\n%s",
			wire.Version, len(wire.Bins), len(wire.Assets), out)
	}
	for name, data := range assets {
		sum := sha256.Sum256([]byte(data))
		if got := wire.Assets[name].SHA256; got != hex.EncodeToString(sum[:]) {
			t.Fatalf("manifest digest for %q = %s, want digest of the on-disk bytes", name, got)
		}
	}
}

func TestBuildUpdateManifestDeterministic(t *testing.T) {
	const version = "0.3.1"
	sums := buildFixtureSums(t, version, fixtureReleaseAssets(version))

	first, err := BuildUpdateManifest(version, sums)
	if err != nil {
		t.Fatalf("BuildUpdateManifest: %v", err)
	}
	second, err := BuildUpdateManifest(version, sums)
	if err != nil {
		t.Fatalf("BuildUpdateManifest: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("generator is not deterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(string(first), `"version": "0.3.1"`) {
		t.Fatalf("output does not carry the version field:\n%s", first)
	}
}

// TestBuildUpdateManifestWithNotesDeterministic extends the determinism pin
// to the notes-bearing variant: the two fields are emitted deterministically
// after the assets map, and the zero-options output stays byte-identical to
// the pre-notes generator.
func TestBuildUpdateManifestWithNotesDeterministic(t *testing.T) {
	const version = "0.3.0"
	sums := buildFixtureSums(t, version, fixtureReleaseAssets(version))
	opts := ManifestOptions{
		NotesURL:    ReleaseNotesURL(version),
		NotesSHA256: strings.Repeat("ab", 32),
	}

	first, err := BuildUpdateManifestWithOptions(version, sums, opts)
	if err != nil {
		t.Fatalf("BuildUpdateManifestWithOptions: %v", err)
	}
	second, err := BuildUpdateManifestWithOptions(version, sums, opts)
	if err != nil {
		t.Fatalf("BuildUpdateManifestWithOptions: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("notes-bearing generator is not deterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	wantURL := "https://sfu-update.snowx.dev/update-notes-0.3.0.md"
	if !strings.Contains(string(first), `"notes_url": "`+wantURL+`"`) {
		t.Fatalf("output missing notes_url %q:\n%s", wantURL, first)
	}
	if !strings.Contains(string(first), `"notes_sha256": "`+strings.Repeat("ab", 32)+`"`) {
		t.Fatalf("output missing notes_sha256:\n%s", first)
	}
	// fields are emitted last, after the assets map
	if strings.Index(string(first), `"notes_url"`) < strings.Index(string(first), `"assets"`) {
		t.Fatalf("notes fields must follow the assets map:\n%s", first)
	}

	// round-trips through the real decode path
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(first)
	}))
	defer srv.Close()
	m, err := fetchLatest(&testHooks{releaseURL: srv.URL}, "sfu", "0.2.0")
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if m.NotesURL != wantURL || m.NotesSHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("decoded notes fields = %q/%q", m.NotesURL, m.NotesSHA256)
	}

	// zero options: byte-identical to the pre-notes two-arg generator, and
	// the notes keys never appear
	plain, err := BuildUpdateManifest(version, sums)
	if err != nil {
		t.Fatalf("BuildUpdateManifest: %v", err)
	}
	zero, err := BuildUpdateManifestWithOptions(version, sums, ManifestOptions{})
	if err != nil {
		t.Fatalf("BuildUpdateManifestWithOptions: %v", err)
	}
	if string(plain) != string(zero) {
		t.Fatalf("zero options changed the manifest bytes:\nplain:\n%s\nzero:\n%s", plain, zero)
	}
	if strings.Contains(string(plain), "notes_") {
		t.Fatalf("pre-notes output must not contain notes fields:\n%s", plain)
	}
}

// TestBuildUpdateManifestNotesPairValidation pins the set-together rule: a
// URL without a hash (or vice versa) and a malformed digest are rejected.
func TestBuildUpdateManifestNotesPairValidation(t *testing.T) {
	const version = "0.3.0"
	sums := buildFixtureSums(t, version, fixtureReleaseAssets(version))

	for _, tc := range []struct {
		name string
		opts ManifestOptions
	}{
		{"url without hash", ManifestOptions{NotesURL: ReleaseNotesURL(version)}},
		{"hash without url", ManifestOptions{NotesSHA256: strings.Repeat("ab", 32)}},
		{"malformed hash", ManifestOptions{NotesURL: ReleaseNotesURL(version), NotesSHA256: "zz"}},
		{"short hash", ManifestOptions{NotesURL: ReleaseNotesURL(version), NotesSHA256: "abcd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildUpdateManifestWithOptions(version, sums, tc.opts); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestBuildUpdateManifestMissingRequiredAsset(t *testing.T) {
	const version = "0.3.1"
	sums := buildFixtureSums(t, version, fixtureReleaseAssets(version))
	delete(sums, "SnowFastSearch-"+version+"-macos-arm64")

	_, err := BuildUpdateManifest(version, sums)
	if err == nil {
		t.Fatal("BuildUpdateManifest succeeded with a missing platform asset")
	}
	if !strings.Contains(err.Error(), "SnowFastSearch-"+version+"-macos-arm64") {
		t.Fatalf("error does not name the missing asset: %v", err)
	}
}

func TestBuildUpdateManifestRejectsEmptyVersion(t *testing.T) {
	sums := buildFixtureSums(t, "0.3.1", fixtureReleaseAssets("0.3.1"))
	if _, err := BuildUpdateManifest("", sums); err == nil {
		t.Fatal("BuildUpdateManifest accepted an empty version")
	}
}

// TestReleasePlatformsCoversAssetSuffix pins the generator's required-asset
// list (releasePlatforms) to the platforms the consumer actually serves
// (assetSuffix): a new consumer platform without a matching releasePlatforms
// entry would make the manifest generator happily emit a manifest whose
// assets map is missing that platform's key.
func TestReleasePlatformsCoversAssetSuffix(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, goarch := range []string{"amd64", "arm64"} {
			suffix, err := assetSuffixFor(goos, goarch)
			if err != nil {
				continue // unsupported platform pair, nothing shipped
			}
			if !slices.Contains(releasePlatforms, suffix) {
				t.Errorf("assetSuffix(%s/%s) = %q is not in releasePlatforms %v — add it or the manifest will miss the asset key", goos, goarch, suffix, releasePlatforms)
			}
		}
	}
}

func TestParseSHA256SUMS(t *testing.T) {
	t.Run("text and binary modes", func(t *testing.T) {
		sums, err := ParseSHA256SUMS(strings.NewReader(
			"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  a\n" +
				"0102999c2519dca95c36c5ffb6a4a7ea1a4e2b7d3d4ba2f5ec4d6e5b09c1a1b6 *b\n"))
		if err != nil {
			t.Fatalf("ParseSHA256SUMS: %v", err)
		}
		if len(sums) != 2 || sums["a"] == "" || sums["b"] == "" {
			t.Fatalf("parsed sums = %v, want entries a and b", sums)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := ParseSHA256SUMS(strings.NewReader("\n")); err == nil {
			t.Fatal("ParseSHA256SUMS accepted an empty file")
		}
	})
	t.Run("malformed line", func(t *testing.T) {
		_, err := ParseSHA256SUMS(strings.NewReader("nothex  file\n"))
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("ParseSHA256SUMS error = %v, want a sha256 format error", err)
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		line := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  dup\n"
		_, err := ParseSHA256SUMS(strings.NewReader(line + line))
		if err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("ParseSHA256SUMS error = %v, want duplicate-name error", err)
		}
	})
	t.Run("short digest", func(t *testing.T) {
		_, err := ParseSHA256SUMS(strings.NewReader("abc  file\n"))
		if err == nil {
			t.Fatal("ParseSHA256SUMS accepted a short digest")
		}
	})
}
