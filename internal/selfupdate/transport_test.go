package selfupdate

// Transport security (R3): asset `url` values travel inside the manifest, so
// a hand-edited or hostile manifest must not be able to downgrade payload
// downloads to plaintext http. The planner rejects non-https URLs as a hard
// manifest error, before any download; loopback hosts are exempt so local
// test servers keep working.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// sfuOnlyManifest returns a manifest whose bins list ships sfu alone with a
// single asset named assetName served from url; sfu is installed on disk so
// the planner plans its update (and therefore validates the URL).
func sfuOnlyManifest(t *testing.T, latest, suffix, assetName, url string) (*updateManifest, string) {
	t.Helper()
	dir := t.TempDir()
	ext := exeExt()
	installSnowFastFixture(t, filepath.Join(dir, "sfu"+ext))
	manifest := &updateManifest{
		Version: latest,
		Bins:    []manifestBin{{Name: "sfu", Prefix: "SnowFastULP"}},
		Assets: map[string]manifestAsset{
			assetName: {SHA256: hexHash([]byte("new-sfu")), URL: url},
		},
	}
	return manifest, dir
}

// TestPlanUpdatesRejectsPlainHTTPAssetURL: a manifest pointing the payload
// at plain http off-loopback is a hard manifest error raised before any
// download could start.
func TestPlanUpdatesRejectsPlainHTTPAssetURL(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	assetName := fmt.Sprintf("SnowFastULP-%s-%s", latest, suffix)
	manifest, dir := sfuOnlyManifest(t, latest, suffix, assetName, "http://updates.example/SnowFastULP")

	_, _, err := planUpdates(manifest, latest, suffix, dir, exeExt())
	if err == nil {
		t.Fatal("expected plain-http asset URL to be rejected, got nil")
	}
	if !strings.Contains(err.Error(), "must be fetched over https") {
		t.Fatalf("expected https error, got: %v", err)
	}
	if !strings.Contains(err.Error(), assetName) {
		t.Fatalf("error must name the offending asset, got: %v", err)
	}
}

// TestPlanUpdatesAcceptsHTTPSAndLoopbackAssetURLs: https URLs are always
// accepted, and plain http survives for loopback hosts (localhost,
// 127.0.0.1, [::1]) so local test servers keep working.
func TestPlanUpdatesAcceptsHTTPSAndLoopbackAssetURLs(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	assetName := fmt.Sprintf("SnowFastULP-%s-%s", latest, suffix)
	for _, url := range []string{
		"https://updates.example/SnowFastULP",
		"http://127.0.0.1:18478/SnowFastULP",
		"http://localhost:8080/SnowFastULP",
		"http://[::1]:8080/SnowFastULP",
	} {
		manifest, dir := sfuOnlyManifest(t, latest, suffix, assetName, url)
		pending, _, err := planUpdates(manifest, latest, suffix, dir, exeExt())
		if err != nil {
			t.Fatalf("url %q: %v", url, err)
		}
		if len(pending) != 1 || pending[0].url != url {
			t.Fatalf("url %q: pending = %+v", url, pending)
		}
	}
}

// TestRequireTransportSecurityNonHTTPSchemes: the scheme check is about
// plaintext http; every non-https scheme is rejected (parse failures too),
// never silently passed to the HTTP client.
func TestRequireTransportSecurityNonHTTPSchemes(t *testing.T) {
	for _, url := range []string{"ftp://updates.example/x", "file:///etc/passwd", "updates.example/x", ""} {
		if err := requireTransportSecurity(url); err == nil {
			t.Errorf("url %q: expected rejection, got nil", url)
		}
	}
}
