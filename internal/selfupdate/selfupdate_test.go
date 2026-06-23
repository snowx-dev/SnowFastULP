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
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.1", "0.1.1", 0},
		{"0.1", "0.1.0", 0},        // missing component == 0
		{"0.2", "0.1.9", 1},        // numeric, not lexical
		{"0.1.9", "0.1.10", -1},    // 9 < 10 numerically
		{"1.0.0", "0.9.9", 1},      // major dominates
		{"0.1.1-dev", "0.1.1", -1}, // prerelease ranks below release
		{"0.1.1", "0.1.1-dev", 1},
		{"0.1.1-dev", "0.1", 1},        // base 0.1.1 > 0.1 despite prerelease
		{"0.1", "0.1.1-dev", -1},       // mirror of above
		{"0.1.1-rc1", "0.1.1-rc2", -1}, // prerelease string order
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFetchSumsParsing(t *testing.T) {
	// Mirror the on-disk SHA256SUMS format (sha256sum output: "<hex>  <name>").
	manifest := "" +
		"aa" + "00" + "112233445566778899aabbccddeeff00112233445566778899aabbccddeeff  SnowFastULP-0.2-linux-amd64\n" +
		"bb112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00  *SnowFastSearch-0.2-windows-amd64.exe\n" +
		"\n" + // blank line tolerated
		"# comment-ish short line\n"

	sums := parseSums([]byte(manifest))
	if len(sums) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(sums), sums)
	}
	want, _ := hex.DecodeString("aa00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if got := sums["SnowFastULP-0.2-linux-amd64"]; string(got) != string(want) {
		t.Errorf("linux digest mismatch: got %x", got)
	}
	// Leading '*' (binary-mode marker) must be stripped from the name.
	if _, ok := sums["SnowFastSearch-0.2-windows-amd64.exe"]; !ok {
		t.Errorf("windows entry not found (star-prefix not stripped?): %v", sums)
	}
}

func TestProductBasename(t *testing.T) {
	if got := productBasename("/opt/bin/sfu"); got != "sfu" {
		t.Fatalf("got %q want sfu", got)
	}
	if got := productBasename("/opt/bin/sfs.exe"); got != "sfs" {
		t.Fatalf("got %q want sfs", got)
	}
}

func TestCheckInvokedBinaryName(t *testing.T) {
	if err := checkInvokedBinaryName("/opt/bin/sfu"); err != nil {
		t.Fatalf("sfu: %v", err)
	}
	if err := checkInvokedBinaryName("/opt/bin/sfs"); err != nil {
		t.Fatalf("sfs: %v", err)
	}
	err := checkInvokedBinaryName("/opt/bin/SnowFastULP-0.1-linux-amd64")
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
	if err := run(nil, "0.1.1", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "already up to date") {
		t.Fatalf("output = %q", buf.String())
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), "old-sfu")
	assertFileContents(t, filepath.Join(dir, "sfs"+exeExt()), "old-sfs")
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
	if err := run(nil, "0.1.1", &buf, &testHooks{
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

func TestRunApplyOrderInvokedBinaryLast(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, []byte("new-sfu"), []byte("new-sfs"))
	defer srv.Close()

	_, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	var applied []string
	recordApplyTarget = func(path string) { applied = append(applied, filepath.Base(path)) }
	t.Cleanup(func() { recordApplyTarget = nil })

	if err := run(nil, "0.1.1", new(bytes.Buffer), &testHooks{
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

	// corrupt the sfu payload on the wire while keeping SHA256SUMS honest
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			writeMockReleaseJSON(w, srv.URL, "0.1.2", suffix, names)
		case "/asset/sfu":
			_, _ = w.Write([]byte("tampered"))
		case "/asset/sfs":
			_, _ = w.Write(goodSFS)
		case "/asset/sums":
			writeMockSums(w, names, goodSFU, goodSFS)
		default:
			http.NotFound(w, r)
		}
	})

	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	err = run(nil, "0.1.1", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), "old-sfu")
	assertFileContents(t, filepath.Join(dir, "sfs"+exeExt()), "old-sfs")
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
	if err := os.WriteFile(sfuPath, []byte("old-sfu"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", &buf, &testHooks{
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
	if err := run(nil, "0.1.2", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "already up to date (0.1.2)") {
		t.Fatalf("output = %q", buf.String())
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), "current-sfu")
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
	err = run(nil, "0.1.0", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: badName,
	})
	if err == nil || !strings.Contains(err.Error(), `SnowFastULP-*  → sfu`) {
		t.Fatalf("expected rename hint, got %v", err)
	}
}

type mockAssetNames struct {
	sfu, sfs string
}

func startMockReleaseServer(t *testing.T, version, suffix string, sfuPayload, sfsPayload []byte) (*httptest.Server, mockAssetNames) {
	t.Helper()
	names := mockAssetNames{
		sfu: fmt.Sprintf("SnowFastULP-%s-%s", version, suffix),
		sfs: fmt.Sprintf("SnowFastSearch-%s-%s", version, suffix),
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
		case "/asset/sums":
			writeMockSums(w, names, sfuPayload, sfsPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	baseURL = srv.URL
	return srv, names
}

func writeMockReleaseJSON(w http.ResponseWriter, base, version, suffix string, names mockAssetNames) {
	rel := ghRelease{TagName: "v" + version}
	if base != "" {
		rel.Assets = []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{
			{Name: names.sfu, URL: base + "/asset/sfu"},
			{Name: names.sfs, URL: base + "/asset/sfs"},
			{Name: sumsAsset, URL: base + "/asset/sums"},
		}
	}
	_ = json.NewEncoder(w).Encode(rel)
}

func writeMockSums(w http.ResponseWriter, names mockAssetNames, sfuPayload, sfsPayload []byte) {
	sfuHash := sha256.Sum256(sfuPayload)
	sfsHash := sha256.Sum256(sfsPayload)
	fmt.Fprintf(w, "%x  %s\n%x  %s\n", sfuHash, names.sfu, sfsHash, names.sfs)
}

func installTestBinaries(t *testing.T, sfuContent, sfsContent, invoked string) (string, *testHooks) {
	t.Helper()
	dir := t.TempDir()
	ext := exeExt()
	sfuPath := filepath.Join(dir, "sfu"+ext)
	sfsPath := filepath.Join(dir, "sfs"+ext)
	mode := os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		mode = 0o644
	}
	if err := os.WriteFile(sfuPath, []byte(sfuContent), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sfsPath, []byte(sfsContent), mode); err != nil {
		t.Fatal(err)
	}
	invokedPath := sfuPath
	if invoked == "sfs" {
		invokedPath = sfsPath
	}
	return dir, &testHooks{executablePath: invokedPath}
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
