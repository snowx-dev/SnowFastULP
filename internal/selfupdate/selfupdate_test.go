package selfupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseManifestHash(t *testing.T) {
	sum := sha256.Sum256([]byte("payload"))
	want := sum[:]
	got, err := parseManifestHash(hex.EncodeToString(want), "SnowFastULP-0.2-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("digest = %x, want %x", got, want)
	}
	if _, err := parseManifestHash("not-a-hash", "bad"); err == nil {
		t.Fatal("expected invalid hash error")
	}
}

func TestPlanUpdatesUsesControlledManifestHashAndURL(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.2.0"
	assetName := fmt.Sprintf("SnowFastULP-%s-%s", latest, suffix)
	payload := []byte("new-sfu")
	sum := sha256.Sum256(payload)

	dir := t.TempDir()
	target := filepath.Join(dir, "sfu"+exeExt())
	installSnowFastFixture(t, target) // existing SnowFast binary → overwrite allowed

	manifest := &updateManifest{
		Version: latest,
		Assets: map[string]manifestAsset{
			assetName: {
				SHA256: hex.EncodeToString(sum[:]),
				URL:    "https://updates.example/sfu",
			},
		},
	}

	pending, _, err := planUpdates(manifest, latest, suffix, dir, exeExt())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if pending[0].url != "https://updates.example/sfu" {
		t.Fatalf("url = %q", pending[0].url)
	}
	if got := hex.EncodeToString(pending[0].hash); got != manifest.Assets[assetName].SHA256 {
		t.Fatalf("hash = %s, want %s", got, manifest.Assets[assetName].SHA256)
	}
}

func TestProductBasename(t *testing.T) {
	if got := productBasename("/opt/bin/sfu"); got != "sfu" {
		t.Fatalf("got %q want sfu", got)
	}
	if got := productBasename("/opt/bin/sfs.exe"); got != "sfs" {
		t.Fatalf("got %q want sfs", got)
	}
	if got := productBasename("/opt/bin/sfl.exe"); got != "sfl" {
		t.Fatalf("got %q want sfl", got)
	}
}

func TestCheckInvokedBinaryName(t *testing.T) {
	known := products
	if err := checkInvokedBinaryName("/opt/bin/sfu", known, "sfu"); err != nil {
		t.Fatalf("sfu: %v", err)
	}
	if err := checkInvokedBinaryName("/opt/bin/sfs", known, "sfs"); err != nil {
		t.Fatalf("sfs: %v", err)
	}
	if err := checkInvokedBinaryName("/opt/bin/sfl", known, "sfl"); err != nil {
		t.Fatalf("sfl: %v", err)
	}
	err := checkInvokedBinaryName("/opt/bin/SnowFastULP-0.1-linux-amd64", known, "sfu")
	if err == nil {
		t.Fatal("expected error for release download name")
	}
	if !strings.Contains(err.Error(), `SnowFastULP-*  → sfu`) {
		t.Fatalf("expected rename hint, got: %v", err)
	}
}

func TestApplyOrderInvokedLast(t *testing.T) {
	pending := []pendingUpdate{
		{bin: "sfu", target: "/bin/sfu"},
		{bin: "sfs", target: "/bin/sfs"},
	}
	order := applyOrder(pending, "sfu")
	if len(order) != 2 || pending[order[0]].bin != "sfs" || pending[order[1]].bin != "sfu" {
		t.Fatalf("order = %v, want sfs then sfu", order)
	}
	order = applyOrder(pending, "sfs")
	if len(order) != 2 || pending[order[0]].bin != "sfu" || pending[order[1]].bin != "sfs" {
		t.Fatalf("order = %v, want sfu then sfs", order)
	}
}

func TestRunAlreadyUpToDate(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.1", suffix, []byte("new-sfu"), []byte("new-sfs"))
	defer srv.Close()

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "already up to date") {
		t.Fatalf("output = %q", buf.String())
	}
	assertFileBytes(t, filepath.Join(dir, "sfu"+exeExt()), fixtureBytes(t))
	assertFileBytes(t, filepath.Join(dir, "sfs"+exeExt()), fixtureBytes(t))
}

func TestRunIntegrationUpdatesBothBinaries(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-0.1.2\n")
	newSFS := []byte("#!/bin/sh\necho sfs-0.1.2\n")
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, newSFU, newSFS)
	defer srv.Close()

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "updated sfu, sfs to 0.1.2") {
		t.Fatalf("output = %q", buf.String())
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), string(newSFU))
	assertFileContents(t, filepath.Join(dir, "sfs"+exeExt()), string(newSFS))
}

func TestRunIntegrationUpdatesThreeBinaries(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-0.1.2\n")
	newSFS := []byte("#!/bin/sh\necho sfs-0.1.2\n")
	newSFL := []byte("#!/bin/sh\necho sfl-0.1.2\n")
	srv, _ := startMockReleaseServerWithSFL(t, "0.1.2", suffix, newSFU, newSFS, newSFL)
	defer srv.Close()

	dir, hooks := installTestBinariesWithSFL(t, "old-sfu", "old-sfs", "old-sfl", "sfl")
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "updated sfu, sfs, sfl to 0.1.2") {
		t.Fatalf("output = %q", buf.String())
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), string(newSFU))
	assertFileContents(t, filepath.Join(dir, "sfs"+exeExt()), string(newSFS))
	assertFileContents(t, filepath.Join(dir, "sfl"+exeExt()), string(newSFL))
}

func TestRunApplyOrderInvokedBinaryLast(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, []byte("new-sfu"), []byte("new-sfs"))
	defer srv.Close()

	_, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	var applied []string
	applyPayloadHook = func(_ []byte, target string, _ []byte) error {
		applied = append(applied, filepath.Base(target))
		return nil
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	if err := run(nil, "0.1.1", "sfu", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want 2 targets", applied)
	}
	if applied[0] != "sfs"+exeExt() || applied[1] != "sfu"+exeExt() {
		t.Fatalf("apply order = %v, want sfs then sfu when invoked as sfu", applied)
	}
}

func TestRunChecksumMismatchLeavesBinariesUntouched(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	goodSFU := []byte("good-sfu")
	goodSFS := []byte("good-sfs")
	srv, names := startMockReleaseServer(t, "0.1.2", suffix, goodSFU, goodSFS)
	defer srv.Close()

	// Corrupt the sfu payload on the wire while keeping the manifest hash honest.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			writeMockReleaseJSON(w, srv.URL, "0.1.2", suffix, names)
		case "/asset/sfu":
			_, _ = w.Write([]byte("tampered"))
		case "/asset/sfs":
			_, _ = w.Write(goodSFS)
		default:
			http.NotFound(w, r)
		}
	})

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	err = run(nil, "0.1.1", "sfu", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	assertFileBytes(t, filepath.Join(dir, "sfu"+exeExt()), fixtureBytes(t))
	assertFileBytes(t, filepath.Join(dir, "sfs"+exeExt()), fixtureBytes(t))
}

func TestRunPartialUpdateReportsVersionSkew(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, []byte("new-sfu"), []byte("new-sfs"))
	defer srv.Close()

	_, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")

	// Invoked as sfu → apply order is [sfs, sfu]. Let the sibling (sfs) apply,
	// then fail the invoked binary (sfu) so the pair ends up version-skewed.
	applyPayloadHook = func(_ []byte, target string, _ []byte) error {
		if productBasename(target) == "sfu" {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	err = run(nil, "0.1.1", "sfu", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	})
	if err == nil {
		t.Fatal("expected a partial-update error")
	}
	msg := err.Error()
	for _, want := range []string{
		"updating sfu failed",
		"already updated to 0.1.2: sfs",
		"still on the old version: sfu",
		"re-run `sfu update`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("partial-update error missing %q:\n%s", want, msg)
		}
	}
}

func TestRunOnlySFUPresentUpdatesSingleBinary(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU := []byte("#!/bin/sh\necho sfu-only\n")
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, newSFU, []byte("unused-sfs"))
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "updated sfu to 0.1.2") {
		t.Fatalf("output = %q", buf.String())
	}
	assertFileContents(t, sfuPath, string(newSFU))
}

func TestRunDowngradeBlockedWhenCurrentNewerThanLatest(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.0", suffix, []byte("old"), []byte("old"))
	defer srv.Close()

	dir, hooks := installTestBinaries(t, "current-sfu", "current-sfs", "sfu")
	var buf bytes.Buffer
	if err := run(nil, "0.1.2", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "already up to date (0.1.2)") {
		t.Fatalf("output = %q", buf.String())
	}
	assertFileBytes(t, filepath.Join(dir, "sfu"+exeExt()), fixtureBytes(t))
}

func TestRunReleaseDownloadNameRejected(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, []byte("x"), []byte("y"))
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	badName := filepath.Join(dir, "SnowFastULP-0.1.1-linux-amd64"+ext)
	if err := os.WriteFile(badName, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = run(nil, "0.1.0", "sfu", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: badName,
	})
	if err == nil || !strings.Contains(err.Error(), `SnowFastULP-*  → sfu`) {
		t.Fatalf("expected rename hint, got %v", err)
	}
}

type mockAssetNames struct {
	sfu, sfs, sfl string
	sfuHash       string
	sfsHash       string
	sflHash       string
}

func startMockReleaseServer(t *testing.T, version, suffix string, sfuPayload, sfsPayload []byte) (*httptest.Server, mockAssetNames) {
	t.Helper()
	return startMockReleaseServerWithSFL(t, version, suffix, sfuPayload, sfsPayload, nil)
}

func startMockReleaseServerWithSFL(t *testing.T, version, suffix string, sfuPayload, sfsPayload, sflPayload []byte) (*httptest.Server, mockAssetNames) {
	t.Helper()
	names := mockAssetNames{
		sfu:     fmt.Sprintf("SnowFastULP-%s-%s", version, suffix),
		sfs:     fmt.Sprintf("SnowFastSearch-%s-%s", version, suffix),
		sfuHash: hexHash(sfuPayload),
		sfsHash: hexHash(sfsPayload),
	}
	if sflPayload != nil {
		names.sfl = fmt.Sprintf("SnowFastLog-%s-%s", version, suffix)
		names.sflHash = hexHash(sflPayload)
	}
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			writeMockReleaseJSON(w, baseURL, version, suffix, names)
		case "/asset/sfu":
			_, _ = w.Write(sfuPayload)
		case "/asset/sfs":
			_, _ = w.Write(sfsPayload)
		case "/asset/sfl":
			if sflPayload == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(sflPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = srv.URL
	return srv, names
}

func writeMockReleaseJSON(w http.ResponseWriter, base, version, suffix string, names mockAssetNames) {
	manifest := updateManifest{
		Version: version,
		Assets:  map[string]manifestAsset{},
		Bins: []manifestBin{
			{Name: "sfu", Prefix: "SnowFastULP"},
			{Name: "sfs", Prefix: "SnowFastSearch"},
		},
	}
	if names.sfl != "" {
		manifest.Bins = append(manifest.Bins, manifestBin{Name: "sfl", Prefix: "SnowFastLog"})
	}
	if base != "" {
		manifest.Assets[names.sfu] = manifestAsset{SHA256: names.sfuHash, URL: base + "/asset/sfu"}
		manifest.Assets[names.sfs] = manifestAsset{SHA256: names.sfsHash, URL: base + "/asset/sfs"}
		if names.sfl != "" {
			manifest.Assets[names.sfl] = manifestAsset{SHA256: names.sflHash, URL: base + "/asset/sfl"}
		}
	}
	_ = json.NewEncoder(w).Encode(manifest)
}

func hexHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// fixtureBinary builds the tiny Go binary from this module tree (package
// main under internal/selfupdate/testdata/buildfixture) with a cache-busting
// -ldflags version so edits to the fixture recompile, and returns its bytes.
// The binary reports "SnowFast" on stdout and carries genuine build info, so
// tests use it as the "existing SnowFast binary" fixture. Build errors fail
// the test; the result is cached for the process. The build lands in a
// fresh unique temp dir that is removed right away — a fixed path under
// os.TempDir() would collide across concurrent `go test` processes.
var fixtureBinary = sync.OnceValues(func() ([]byte, error) {
	// Anchor on this test file so the helper works regardless of the test
	// process's working directory (go test does not chdir to the module root).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("cannot locate selfupdate_test.go for fixture path")
	}
	moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	pkg := filepath.Join("internal", "selfupdate", "testdata", "buildfixture")
	tmpDir, err := os.MkdirTemp("", "snowfast-selfupdate-fixture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	out := filepath.Join(tmpDir, "snowfast-selfupdate-fixture")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	v := fmt.Sprintf("fixture-%d", time.Now().UnixNano())
	cmd := exec.Command("go", "build", "-o", out, "-ldflags", "-X main.fixtureVersion="+v, "./"+pkg)
	cmd.Dir = moduleRoot
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("building fixture: %v\n%s", err, b)
	}
	return os.ReadFile(out)
})

// buildSnowFastFixture returns the path of a fresh copy of the fixture
// binary inside t's temp dir (removed by t.Cleanup). Copying per test keeps
// the shared build out of os.TempDir(), where a fixed path would collide
// across concurrent `go test` processes, and the byte copy preserves the
// build info the overwrite guard checks.
func buildSnowFastFixture(t *testing.T) (string, error) {
	t.Helper()
	data, err := fixtureBinary()
	if err != nil {
		return "", err
	}
	path := filepath.Join(t.TempDir(), "snowfast-selfupdate-fixture"+exeExt())
	if err := os.WriteFile(path, data, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// installSnowFastFixture copies the built fixture binary over path, creating
// parent dirs as needed. mode defaults to 0o755.
func installSnowFastFixture(t *testing.T, path string) {
	t.Helper()
	data, err := fixtureBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mode := os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		mode = 0o644
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func installTestBinaries(t *testing.T, sfuContent, sfsContent, invoked string) (string, *testHooks) {
	t.Helper()
	return installTestBinariesWithSFL(t, sfuContent, sfsContent, "", invoked)
}

// installTestBinariesWithSFL installs genuine SnowFast binaries (built from
// this module tree) as the existing on-disk set: the overwrite guard only
// allows swapping targets that carry this module's build info, so run-level
// tests must start from real SnowFast binaries. sfuContent/sfsContent/
// sflContent are kept in the signature for payload distinction in failure
// messages but the files receive the fixture binary.
func installTestBinariesWithSFL(t *testing.T, sfuContent, sfsContent, sflContent, invoked string) (string, *testHooks) {
	t.Helper()
	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	sfsPath := filepath.Join(dir, "sfs"+ext)
	sflPath := filepath.Join(dir, "sfl"+ext)
	installSnowFastFixture(t, sfuPath)
	installSnowFastFixture(t, sfsPath)
	if sflContent != "" {
		installSnowFastFixture(t, sflPath)
	}
	invokedPath := sfuPath
	if invoked == "sfs" {
		invokedPath = sfsPath
	}
	if invoked == "sfl" {
		invokedPath = sflPath
	}
	return dir, &testHooks{executablePath: invokedPath}
}

// fixtureBytes returns the raw bytes of the built SnowFast fixture binary,
// for asserting a file was left untouched in its genuine-SnowFast form.
func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	src, err := buildSnowFastFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertFileBytes fails when path does not hold exactly the given bytes.
func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %d bytes, want %d matching the fixture", path, len(got), len(want))
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, string(got), want)
	}
}

// mockBin describes one binary served by startMockReleaseServerWithBins.
type mockBin struct {
	bin     string // on-disk name, e.g. "sfx"
	prefix  string // release asset prefix, e.g. "SnowFastX"
	payload []byte
}

// startMockReleaseServerWithBins serves an arbitrary set of binaries and
// emits a manifest carrying a `bins` array (the new schema path). Each bin's
// asset URL is base+"/asset/"+bin.
func startMockReleaseServerWithBins(t *testing.T, version, suffix string, bins []mockBin) *httptest.Server {
	t.Helper()
	manifest := updateManifest{
		Version: version,
		Assets:  map[string]manifestAsset{},
	}
	for _, b := range bins {
		manifest.Bins = append(manifest.Bins, manifestBin{Name: b.bin, Prefix: b.prefix})
		assetName := fmt.Sprintf("%s-%s-%s", b.prefix, version, suffix)
		manifest.Assets[assetName] = manifestAsset{
			SHA256: hexHash(b.payload),
		}
	}
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/latest":
			_ = json.NewEncoder(w).Encode(manifest)
		case strings.HasPrefix(r.URL.Path, "/asset/"):
			binName := strings.TrimPrefix(r.URL.Path, "/asset/")
			for _, b := range bins {
				if b.bin == binName {
					_, _ = w.Write(b.payload)
					return
				}
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = srv.URL
	// Patch asset URLs now that the base is known.
	for _, b := range bins {
		assetName := fmt.Sprintf("%s-%s-%s", b.prefix, version, suffix)
		manifest.Assets[assetName] = manifestAsset{
			SHA256: hexHash(b.payload),
			URL:    baseURL + "/asset/" + b.bin,
		}
	}
	return srv
}

func TestResolveProductsPrefersManifestBins(t *testing.T) {
	m := &updateManifest{
		Bins: []manifestBin{
			{Name: "sfu", Prefix: "SnowFastULP"},
			{Name: "sfx", Prefix: "SnowFastX"},
		},
	}
	got, err := resolveProducts(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(products)+1 {
		t.Fatalf("union len = %d, want %d", len(got), len(products)+1)
	}
	if got[0] != products[0] || got[1] != products[1] || got[2] != products[2] {
		t.Fatalf("trio prefix = %+v", got[:3])
	}
	if got[3].bin != "sfx" || got[3].prefix != "SnowFastX" {
		t.Fatalf("sfx = %+v", got[3])
	}

	got, err = resolveProducts(&updateManifest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(products) {
		t.Fatalf("fallback len = %d, want %d", len(got), len(products))
	}
	for i, p := range products {
		if got[i] != p {
			t.Fatalf("fallback[%d] = %+v, want %+v", i, got[i], p)
		}
	}
}

func TestPlanUpdatesInstallsMissingBin(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("new-sfu")},
		{"sfs", "SnowFastSearch", []byte("new-sfs")},
		{"sfl", "SnowFastLog", []byte("new-sfl")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	// Only sfu exists on disk; sfs and sfl are missing.
	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed

	manifest := &updateManifest{
		Version: latest,
		Assets:  map[string]manifestAsset{},
	}
	for _, b := range bins {
		assetName := fmt.Sprintf("%s-%s-%s", b.prefix, latest, suffix)
		manifest.Assets[assetName] = manifestAsset{
			SHA256: hexHash(b.payload),
			URL:    srv.URL + "/asset/" + b.bin,
		}
		manifest.Bins = append(manifest.Bins, manifestBin{Name: b.bin, Prefix: b.prefix})
	}

	pending, _, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending len = %d, want 3", len(pending))
	}
	byBin := make(map[string]bool, len(pending))
	for _, u := range pending {
		byBin[u.bin] = true
		if u.bin == "sfu" && u.isNew {
			t.Errorf("sfu should be isNew=false (exists on disk)")
		}
		if (u.bin == "sfs" || u.bin == "sfl") && !u.isNew {
			t.Errorf("%s should be isNew=true (missing on disk)", u.bin)
		}
	}
	for _, want := range []string{"sfu", "sfs", "sfl"} {
		if !byBin[want] {
			t.Errorf("pending missing %s", want)
		}
	}
}

// manifestForBins builds an updateManifest over the mock server's bins.
func manifestForBins(srv *httptest.Server, latest, suffix string, bins []mockBin) *updateManifest {
	manifest := &updateManifest{
		Version: latest,
		Assets:  map[string]manifestAsset{},
	}
	for _, b := range bins {
		assetName := fmt.Sprintf("%s-%s-%s", b.prefix, latest, suffix)
		manifest.Assets[assetName] = manifestAsset{
			SHA256: hexHash(b.payload),
			URL:    srv.URL + "/asset/" + b.bin,
		}
		manifest.Bins = append(manifest.Bins, manifestBin{Name: b.bin, Prefix: b.prefix})
	}
	return manifest
}

// TestPlanUpdatesRefusesForeignExtraBin proves an existing non-SnowFast file
// at a manifest extra bin's target is refused, regardless of any stamp.
func TestPlanUpdatesRefusesForeignExtraBin(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("new-sfu")},
		{"sfx", "SnowFastX", []byte("new-sfx")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	installSnowFastFixture(t, filepath.Join(dir, "sfu"+ext)) // existing SnowFast binary → overwrite allowed
	sfxPath := filepath.Join(dir, "sfx"+ext)
	if err := os.WriteFile(sfxPath, []byte("keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := manifestForBins(srv, latest, suffix, bins)

	_, _, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v, want foreign-file overwrite refusal", err)
	}
}

// TestPlanUpdatesAllowsExistingExtraBin proves an existing SnowFast extra
// binary is overwritten in place (isNew=false), not treated as a fresh install.
func TestPlanUpdatesAllowsExistingExtraBin(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("new-sfu")},
		{"sfx", "SnowFastX", []byte("new-sfx")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	installSnowFastFixture(t, filepath.Join(dir, "sfu"+ext)) // existing SnowFast binary → overwrite allowed
	installSnowFastFixture(t, filepath.Join(dir, "sfx"+ext)) // existing SnowFast extra → overwrite allowed

	manifest := manifestForBins(srv, latest, suffix, bins)

	pending, _, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %+v, want sfu and sfx", pending)
	}
	got := make(map[string]bool, len(pending))
	for _, u := range pending {
		got[u.bin] = u.isNew
	}
	if len(got) != 2 {
		t.Fatalf("pending = %+v, want distinct bins", pending)
	}
	if got["sfx"] {
		t.Fatal("existing sfx should be isNew=false (overwrite, not fresh install)")
	}
	if got["sfu"] {
		t.Fatal("existing sfu should be isNew=false")
	}
}

func TestRunInstallsNewBin(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	newSFX := []byte("#!/bin/sh\necho sfx-0.4\n")
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("#!/bin/sh\necho sfu-0.4\n")},
		{"sfx", "SnowFastX", newSFX},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed

	var buf bytes.Buffer
	if err := run(nil, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(buf.String(), "new tool available: sfx") {
		t.Errorf("output missing new-tool narration:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "updated sfu to 0.4") {
		t.Errorf("output missing updated sfu:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "installed new: sfx to 0.4") {
		t.Errorf("output missing installed new sfx:\n%s", buf.String())
	}

	// sfx must now exist on disk with the new payload and 0o755 perms.
	sfxPath := filepath.Join(dir, "sfx"+ext)
	got, err := os.ReadFile(sfxPath)
	if err != nil {
		t.Fatalf("read sfx: %v", err)
	}
	if string(got) != string(newSFX) {
		t.Fatalf("sfx = %q, want %q", got, newSFX)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(sfxPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("sfx perms = %o, want 755", info.Mode().Perm())
		}
	}
}

func TestRunDryRunPrintsPlanNoWrite(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	newSFX := []byte("#!/bin/sh\necho sfx-0.4\n")
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("#!/bin/sh\necho sfu-0.4\n")},
		{"sfx", "SnowFastX", newSFX},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed

	var buf bytes.Buffer
	if err := run([]string{"--dry-run"}, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"would update: sfu (to 0.4)",
		"would install new: sfx",
		"no changes made (dry run)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	// Dry-run must NOT claim an action it won't perform.
	if strings.Contains(out, "installing") {
		t.Errorf("dry-run output should not contain \"installing\":\n%s", out)
	}

	// Nothing written: sfu still the genuine SnowFast binary, sfx absent.
	assertFileBytes(t, sfuPath, fixtureBytes(t))
	if _, err := os.Stat(filepath.Join(dir, "sfx"+ext)); !os.IsNotExist(err) {
		t.Errorf("sfx should not exist after dry-run, got err=%v", err)
	}
}

func TestRunPartialNewBinFailureReportsSkew(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("#!/bin/sh\necho sfu-0.4\n")},
		{"sfs", "SnowFastSearch", []byte("#!/bin/sh\necho sfs-0.4\n")},
		{"sfx", "SnowFastX", []byte("#!/bin/sh\necho sfx-0.4\n")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed
	sfsPath := filepath.Join(dir, "sfs"+ext)
	installSnowFastFixture(t, sfsPath) // existing SnowFast binary → overwrite allowed

	// Invoked as sfu → apply order is [sfs, sfx, sfu]. Let sfs apply, then fail
	// sfx (new bin) so the toolkit ends up skewed: sfs updated, sfx not
	// installed, sfu still old. The skew message must report all three.
	applyPayloadHook = func(_ []byte, target string, _ []byte) error {
		if productBasename(target) == "sfx" {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	var buf bytes.Buffer
	err := run(nil, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	})
	if err == nil {
		t.Fatal("expected a partial-install error")
	}
	msg := err.Error()
	for _, want := range []string{
		"installing new sfx failed",
		"already updated to 0.4: sfs",
		"still on the old version: sfu",
		"new bin not installed: sfx",
		"re-run `sfu update`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("partial-install error missing %q:\n%s", want, msg)
		}
	}
	// sfu untouched, sfx not installed.
	assertFileBytes(t, sfuPath, fixtureBytes(t))
	if _, err := os.Stat(filepath.Join(dir, "sfx"+ext)); !os.IsNotExist(err) {
		t.Errorf("sfx should not exist after failed install, got err=%v", err)
	}
}

// TestRunInstallsMissingExtraBin proves a manifest extra bin absent from a
// fresh install dir (no stamp, no flag) is installed next to the running
// binary as an ordinary pending update.
func TestRunInstallsMissingExtraBin(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("#!/bin/sh\necho sfu-0.4\n")},
		{"sfx", "SnowFastX", []byte("#!/bin/sh\necho sfx-0.4\n")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	// Fresh dir: only sfu exists; sfx was never installed here.
	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath)

	var buf bytes.Buffer
	if err := run(nil, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"new tool available: sfx",
		"updated sfu to 0.4",
		"installed new: sfx to 0.4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	assertFileContents(t, sfuPath, string(bins[0].payload))
	assertFileContents(t, filepath.Join(dir, "sfx"+ext), string(bins[1].payload))
}

// TestRunRefusesForeignExistingExtraBin proves the full run refuses a
// foreign file at an extra bin's target instead of clobbering it.
func TestRunRefusesForeignExistingExtraBin(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("#!/bin/sh\necho sfu-0.4\n")},
		{"sfx", "SnowFastX", []byte("#!/bin/sh\necho sfx-0.4\n")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	sfxPath := filepath.Join(dir, "sfx"+ext)
	installSnowFastFixture(t, sfuPath)
	if err := os.WriteFile(sfxPath, []byte("precious user data"), 0o755); err != nil {
		t.Fatal(err)
	}

	var appliedTargets []string
	applyPayloadHook = func(_ []byte, target string, _ []byte) error {
		appliedTargets = append(appliedTargets, target)
		return nil
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	var buf bytes.Buffer
	if err := run(nil, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	}); err == nil {
		t.Fatal("expected refusal for the foreign sfx")
	} else if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
	// Nothing may be applied — planning failed before any swap.
	if len(appliedTargets) != 0 {
		t.Fatalf("applied = %v, want none", appliedTargets)
	}
	got, err := os.ReadFile(sfxPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "precious user data" {
		t.Fatalf("sfx = %q, want untouched", got)
	}
}

func TestSkewErrorDistinguishesUpdatedFromInstalled(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("#!/bin/sh\necho sfu-0.4\n")},
		{"sfx", "SnowFastX", []byte("#!/bin/sh\necho sfx-0.4\n")},
		{"sfy", "SnowFastY", []byte("#!/bin/sh\necho sfy-0.4\n")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed

	// Invoked as sfu → apply order is [sfx, sfy, sfu]. Let sfx (new) install
	// succeed, then fail sfy (new) so the toolkit skews: sfx installed, sfy
	// not installed, sfu still old. The skew message must label sfx as
	// "already installed new", NOT "already updated".
	applyPayloadHook = func(_ []byte, target string, _ []byte) error {
		if productBasename(target) == "sfy" {
			return fmt.Errorf("disk full")
		}
		return nil
	}
	t.Cleanup(func() { applyPayloadHook = nil })

	var buf bytes.Buffer
	err := run(nil, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	})
	if err == nil {
		t.Fatal("expected a partial-install error")
	}
	msg := err.Error()
	for _, want := range []string{
		"installing new sfy failed",
		"already installed new: sfx",
		"still on the old version: sfu",
		"new bin not installed: sfy",
		"re-run `sfu update`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("skew error missing %q:\n%s", want, msg)
		}
	}
	// The mislabeled form must NOT appear.
	if strings.Contains(msg, "already updated to 0.4: sfx") {
		t.Errorf("skew error mislabels installed sfx as updated:\n%s", msg)
	}
}

func TestPlanUpdatesStatErrorSurfaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based stat error test is Unix-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod 000")
	}
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("new-sfu")},
	}
	srv := startMockReleaseServerWithBins(t, latest, suffix, bins)
	defer srv.Close()

	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed

	manifest := &updateManifest{
		Version: latest,
		Assets:  map[string]manifestAsset{},
	}
	for _, b := range bins {
		assetName := fmt.Sprintf("%s-%s-%s", b.prefix, latest, suffix)
		manifest.Assets[assetName] = manifestAsset{
			SHA256: hexHash(b.payload),
			URL:    srv.URL + "/asset/" + b.bin,
		}
		manifest.Bins = append(manifest.Bins, manifestBin{Name: b.bin, Prefix: b.prefix})
	}

	// Make the parent dir non-traversable so os.Stat returns a non-IsNotExist error.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, _, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err == nil {
		t.Fatal("expected stat error, got nil")
	}
	if !strings.Contains(err.Error(), "stat ") {
		t.Fatalf("expected stat error, got: %v", err)
	}
}

func TestPlanUpdatesSkipsNewBinMissingAsset(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	// Manifest declares sfx but provides NO asset for it.
	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	installSnowFastFixture(t, sfuPath) // existing SnowFast binary → overwrite allowed
	manifest := &updateManifest{
		Version: latest,
		Bins: []manifestBin{
			{Name: "sfu", Prefix: "SnowFastULP"},
			{Name: "sfx", Prefix: "SnowFastX"},
		},
		Assets: map[string]manifestAsset{
			fmt.Sprintf("SnowFastULP-%s-%s", latest, suffix): {
				SHA256: hexHash([]byte("new-sfu")),
				URL:    "https://example/sfu",
			},
			// No SnowFastX asset on purpose.
		},
	}

	pending, _, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err != nil {
		t.Fatalf("planUpdates: %v", err)
	}
	if len(pending) != 1 || pending[0].bin != "sfu" || pending[0].isNew {
		t.Fatalf("pending = %+v, want only sfu (sfx skipped, no asset)", pending)
	}
}

func TestResolveProductsDedupAndRejectsEmpty(t *testing.T) {
	m := &updateManifest{
		Bins: []manifestBin{
			{Name: "sfu", Prefix: "SnowFastULP"},
			{Name: "sfu", Prefix: "SnowFastULP"}, // same prefix: ignore
			{Name: "", Prefix: "SnowFastEmpty"},
			{Name: "sfx", Prefix: ""},
			{Name: "../evil", Prefix: "SnowFastX"},
			{Name: `C:\foo`, Prefix: "SnowFastX"},
			{Name: "SFU", Prefix: "SnowFastULP"}, // normalizes, same prefix
			{Name: "sfy", Prefix: "SnowFastY"},
		},
	}
	got, err := resolveProducts(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(products)+1 {
		t.Fatalf("len = %d, want trio+sfy: %+v", len(got), got)
	}
	if got[0].prefix != "SnowFastULP" {
		t.Fatalf("sfu prefix = %q", got[0].prefix)
	}
	if got[len(got)-1].bin != "sfy" || got[len(got)-1].prefix != "SnowFastY" {
		t.Fatalf("last = %+v, want sfy", got[len(got)-1])
	}

	_, err = resolveProducts(&updateManifest{
		Bins: []manifestBin{
			{Name: "sfu", Prefix: "SnowFastULP"},
			{Name: "sfu", Prefix: "SnowFastOTHER"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting prefixes") {
		t.Fatalf("expected prefix conflict, got %v", err)
	}

	_, err = resolveProducts(&updateManifest{
		Bins: []manifestBin{{Name: "", Prefix: ""}},
	})
	if err == nil || !strings.Contains(err.Error(), "no valid entries") {
		t.Fatalf("expected all-invalid error, got %v", err)
	}
}

func TestCheckInvokedBinaryNameManifestDriven(t *testing.T) {
	known := []product{
		{bin: "sfu", prefix: "SnowFastULP"},
		{bin: "sfx", prefix: "SnowFastX"},
	}
	// sfx is manifest-declared → allowed.
	if err := checkInvokedBinaryName("/opt/bin/sfx", known, "sfx"); err != nil {
		t.Fatalf("sfx should be allowed: %v", err)
	}
	// Release download name → rejected, message lists known bins.
	err := checkInvokedBinaryName("/opt/bin/SnowFastULP-0.1-linux-amd64", known, "sfu")
	if err == nil {
		t.Fatal("expected error for release download name")
	}
	for _, want := range []string{"sfu", "sfx", "SnowFastULP-*"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestRunUpdateHelpReachesUpdateUsage(t *testing.T) {
	var buf bytes.Buffer
	if err := run([]string{"--help"}, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     "https://example.invalid",
		executablePath: "/tmp/sfu",
	}); err != nil {
		t.Fatalf("run --help: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "usage: sfu update") {
		t.Errorf("update --help missing usage line:\n%s", out)
	}

	buf.Reset()
	if err := run([]string{"-help"}, "0.3", "sfs", &buf, nil); err != nil {
		t.Fatalf("run -help: %v", err)
	}
	if !strings.Contains(buf.String(), "usage: sfs update") {
		t.Errorf("-help missing sfs usage:\n%s", buf.String())
	}
}

func TestDispatchUpdateHelp(t *testing.T) {
	var buf bytes.Buffer
	handled, err := Dispatch([]string{"update", "--help"}, "0.3", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("Dispatch(update --help) handled=false")
	}
	if !strings.Contains(buf.String(), "update [--dry-run]") {
		t.Errorf("Dispatch update --help missing subcommand usage:\n%s", buf.String())
	}

	buf.Reset()
	handled, err = Dispatch([]string{"--help"}, "0.3", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("Dispatch(--help) should be handled=false so main prints generic help")
	}
}

func TestUserAgent(t *testing.T) {
	cases := []struct {
		bin, version, want string
	}{
		{"sfu", "0.2.0", "SnowFastULP-selfupdate/0.2.0 (sfu)"},
		{"sfl", "0.2-dev", "SnowFastULP-selfupdate/0.2-dev (sfl)"},
		{"sfu", "", "SnowFastULP-selfupdate/unknown (sfu)"},
		{"", "0.2.0", "SnowFastULP-selfupdate/0.2.0 (unknown)"},
		{"my sfu", "0.2-dev", "SnowFastULP-selfupdate/0.2-dev (unknown)"},
		{".", "0.2-dev", "SnowFastULP-selfupdate/0.2-dev (unknown)"},
		{"sfu", "0.2 dev", "SnowFastULP-selfupdate/unknown (sfu)"},
		{"", "", "SnowFastULP-selfupdate/unknown (unknown)"},
	}
	for _, tc := range cases {
		if got := userAgent(tc.bin, tc.version); got != tc.want {
			t.Errorf("userAgent(%q, %q) = %q, want %q", tc.bin, tc.version, got, tc.want)
		}
	}
}

func TestRunUpdateSendsUserAgentOnManifestAndAsset(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	sfuPayload := []byte("new-sfu")
	sfsPayload := []byte("new-sfs")
	names := mockAssetNames{
		sfu:     fmt.Sprintf("SnowFastULP-0.1.2-%s", suffix),
		sfs:     fmt.Sprintf("SnowFastSearch-0.1.2-%s", suffix),
		sfuHash: hexHash(sfuPayload),
		sfsHash: hexHash(sfsPayload),
	}
	// Requests from run() are strictly sequential (manifest, then each asset),
	// so a plain map cannot race.
	ua := map[string]string{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua[r.URL.Path] = r.UserAgent()
		switch r.URL.Path {
		case "/releases/latest":
			writeMockReleaseJSON(w, srv.URL, "0.1.2", suffix, names)
		case "/asset/sfu":
			_, _ = w.Write(sfuPayload)
		case "/asset/sfs":
			_, _ = w.Write(sfsPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	_, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	// invokedBin comes from productBasename(hooks.executablePath) = "sfu".
	want := "SnowFastULP-selfupdate/0.1.1 (sfu)"
	for _, path := range []string{"/releases/latest", "/asset/sfu", "/asset/sfs"} {
		if got := ua[path]; got != want {
			t.Errorf("User-Agent for %s = %q, want %q", path, got, want)
		}
	}
}
