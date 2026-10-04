package selfupdate

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- N5: permission hint ---

// TestWithPermHintWrapsEACCES proves the hint is appended to permission
// errors with the original text preserved, and that unrelated errors pass
// through unchanged.
func TestWithPermHintWrapsEACCES(t *testing.T) {
	base := fmt.Errorf("install new binary: %w", os.ErrPermission)
	got := withPermHint(base)
	if !errors.Is(got, os.ErrPermission) {
		t.Fatalf("errors.Is lost after wrapping: %v", got)
	}
	if !strings.Contains(got.Error(), "install new binary") {
		t.Fatalf("original text lost: %v", got)
	}
	for _, want := range []string{"read-only or root-owned", "rebuild the image"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("hint missing %q:\n%v", want, got)
		}
	}

	other := fmt.Errorf("no such file: %w", os.ErrNotExist)
	if withPermHint(other) != error(other) {
		t.Fatalf("non-permission error was rewritten: %v", withPermHint(other))
	}

	// The raw errno shape callers see from os.Rename/rename(2) on Linux.
	errnoErr := &os.PathError{Op: "rename", Path: "/x", Err: syscall.EACCES}
	hinted := withPermHint(errnoErr)
	if hinted == error(errnoErr) {
		t.Fatalf("EACCES errno error did not get a hint: %v", hinted)
	}
	if !strings.Contains(hinted.Error(), "rebuild the image") {
		t.Fatalf("EACCES hint missing: %v", hinted)
	}
}

// TestAcquireUpdateLockPermHint is the S-06/R4 regression: a read-only
// install dir fails at lock creation, and that error used to surface as a
// bare `create update lock: … permission denied` with none of the actionable
// container hint the apply path already gets from withPermHint.
func TestAcquireUpdateLockPermHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read-only test is Unix-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod 000")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := acquireUpdateLock(dir)
	if err == nil {
		t.Fatal("expected lock creation to fail in a read-only dir, got nil")
	}
	if !strings.Contains(err.Error(), "create update lock") {
		t.Fatalf("expected the create update lock error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "the install directory may be read-only or root-owned") {
		t.Fatalf("lock-creation error missing the permission hint: %v", err)
	}
}

// --- N6: orphan notice ---

// TestPlanUpdatesReportsOrphans proves a retired trio bin — one the release
// no longer ships, so it is absent from both the manifest bins and the
// hardcoded set — that is still installed on disk is reported as an orphan,
// and that an absent-on-disk trio bin is not.
func TestPlanUpdatesReportsOrphans(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	dir := t.TempDir()
	ext := exeExt()
	// sfs installed on disk, but the manifest no longer ships it (only sfu
	// appears in bins); sfl is likewise absent from bins but not installed.
	installSnowFastFixture(t, filepath.Join(dir, "sfs"+ext))

	manifest := &updateManifest{
		Version: latest,
		Bins:    []manifestBin{{Name: "sfu", Prefix: "SnowFastULP"}},
		Assets: map[string]manifestAsset{
			"SnowFastULP-" + latest + "-" + suffix:    {SHA256: hexHash([]byte("new-sfu")), URL: "https://example/sfu"},
			"SnowFastSearch-" + latest + "-" + suffix: {SHA256: hexHash([]byte("new-sfs")), URL: "https://example/sfs"},
			"SnowFastLog-" + latest + "-" + suffix:    {SHA256: hexHash([]byte("new-sfl")), URL: "https://example/sfl"},
		},
	}
	_, orphans, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err != nil {
		t.Fatal(err)
	}
	// sfs installed but retired manifest-side; sfl retired too but absent
	// from disk, so only sfs is reported.
	if len(orphans) != 1 || orphans[0] != "sfs" {
		t.Fatalf("orphans = %v, want [sfs]", orphans)
	}

	// Nothing installed in an empty dir → no orphans.
	_, orphans, err = planUpdates(manifest, latest, suffix, t.TempDir(), ext)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("orphans for absent bin = %v, want none", orphans)
	}
}

// TestPlanUpdatesNoOrphansForLookalikes proves orphan detection only matches
// the hardcoded trio: foreign files named git/rg (and a manifest-declared
// extra like sfx) are never reported, and trio bins present in the manifest
// are not orphans either.
func TestPlanUpdatesNoOrphansForLookalikes(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	dir := t.TempDir()
	ext := exeExt()
	for _, lookalike := range []string{"git", "rg", "sfx"} {
		if err := os.WriteFile(filepath.Join(dir, lookalike+ext), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manifest := &updateManifest{
		Version: latest,
		Bins: []manifestBin{
			{Name: "sfu", Prefix: "SnowFastULP"},
			{Name: "sfs", Prefix: "SnowFastSearch"},
			{Name: "sfl", Prefix: "SnowFastLog"},
		},
		Assets: map[string]manifestAsset{},
	}
	_, orphans, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("orphans = %v, want none (trio all in manifest; lookalikes never match)", orphans)
	}
}

// TestRunPrintsOrphanNotice wires the notice through the run summary: sfs and
// sfl exist on disk, the manifest ships only sfu, and the run must say so
// while still updating sfu. The orphans themselves are retired: they stay on
// the old version — their assets are not demanded and their payload URLs in
// the manifest (if any) are not fetched.
func TestRunPrintsOrphanNotice(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	payload := []byte("new-sfu")
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			// Manifest ships only sfu; sfs/sfl exist on disk as orphans.
			fmt.Fprintf(w, `{"version":"0.1.2","bins":[{"name":"sfu","prefix":"SnowFastULP"}],"assets":{"SnowFastULP-0.1.2-%s":{"sha256":%q,"url":%q},"SnowFastSearch-0.1.2-%s":{"sha256":%q,"url":%q},"SnowFastLog-0.1.2-%s":{"sha256":%q,"url":%q}}}`,
				suffix, hexHash(payload), baseURL+"/asset/sfu",
				suffix, hexHash([]byte("new-sfs")), baseURL+"/asset/sfs",
				suffix, hexHash([]byte("new-sfl")), baseURL+"/asset/sfl")
		case "/asset/sfu":
			_, _ = w.Write(payload)
		case "/asset/sfs":
			_, _ = w.Write([]byte("new-sfs"))
		case "/asset/sfl":
			_, _ = w.Write([]byte("new-sfl"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	baseURL = srv.URL

	dir, hooks := installTestBinariesWithSFL(t, "old-sfu", "old-sfs", "old-sfl", "sfu")
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"note: sfs is installed here but no longer receives updates (absent from the update manifest)",
		"note: sfl is installed here but no longer receives updates (absent from the update manifest)",
		"updated sfu to 0.1.2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "updated sfu, sfs") || strings.Contains(out, "sfl to 0.1.2") {
		t.Errorf("orphans must not be updated, got:\n%s", out)
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), string(payload))
	// The retired bins keep their pre-update bytes: absent from bins means
	// excluded from the plan, not "updated anyway when the asset exists".
	assertFileBytes(t, filepath.Join(dir, "sfs"+exeExt()), fixtureBytes(t))
	assertFileBytes(t, filepath.Join(dir, "sfl"+exeExt()), fixtureBytes(t))
}

// TestRunOrphanWithNoAssetStillUpdatesRest is the G-04 regression: the
// manifest's bins list drops sfl and (correctly) publishes no SnowFastLog
// asset either. Before the orphan exclusion the whole update failed with
// `has no asset "SnowFastLog-…" for this platform` and the orphan note was
// unreachable; now sfu/sfs update, the note prints, and sfl keeps its bytes.
func TestRunOrphanWithNoAssetStillUpdatesRest(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	sfuPayload, sfsPayload := []byte("new-sfu"), []byte("new-sfs")
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			fmt.Fprintf(w, `{"version":"0.1.2","bins":[{"name":"sfu","prefix":"SnowFastULP"},{"name":"sfs","prefix":"SnowFastSearch"}],"assets":{"SnowFastULP-0.1.2-%s":{"sha256":%q,"url":%q},"SnowFastSearch-0.1.2-%s":{"sha256":%q,"url":%q}}}`,
				suffix, hexHash(sfuPayload), baseURL+"/asset/sfu",
				suffix, hexHash(sfsPayload), baseURL+"/asset/sfs")
		case "/asset/sfu":
			_, _ = w.Write(sfuPayload)
		case "/asset/sfs":
			_, _ = w.Write(sfsPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	baseURL = srv.URL

	dir, hooks := installTestBinariesWithSFL(t, "old-sfu", "old-sfs", "old-sfl", "sfu")
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"note: sfl is installed here but no longer receives updates (absent from the update manifest)",
		"updated sfu, sfs to 0.1.2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), string(sfuPayload))
	assertFileContents(t, filepath.Join(dir, "sfs"+exeExt()), string(sfsPayload))
	assertFileBytes(t, filepath.Join(dir, "sfl"+exeExt()), fixtureBytes(t))
}

// TestRunOrphanAbsentFromDiskGetsNoNotes proves the missing-sibling note
// (see the missingCore plumbing) and the orphan note never double-report the
// same bin: a trio bin absent from both the bins list and the disk is
// silently irrelevant — it is not installed, so there is nothing to notify
// about.
func TestRunOrphanAbsentFromDiskGetsNoNotes(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	sfuPayload, sfsPayload := []byte("new-sfu"), []byte("new-sfs")
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			fmt.Fprintf(w, `{"version":"0.1.2","bins":[{"name":"sfu","prefix":"SnowFastULP"},{"name":"sfs","prefix":"SnowFastSearch"}],"assets":{"SnowFastULP-0.1.2-%s":{"sha256":%q,"url":%q},"SnowFastSearch-0.1.2-%s":{"sha256":%q,"url":%q}}}`,
				suffix, hexHash(sfuPayload), baseURL+"/asset/sfu",
				suffix, hexHash(sfsPayload), baseURL+"/asset/sfs")
		case "/asset/sfu":
			_, _ = w.Write(sfuPayload)
		case "/asset/sfs":
			_, _ = w.Write(sfsPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	baseURL = srv.URL

	// sfl is neither installed nor in the manifest bins; only sfu/sfs exist.
	dir, hooks := installTestBinaries(t, "old-sfu", "old-sfs", "sfu")
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "note: sfl") {
		t.Errorf("absent bin must not be reported, got:\n%s", out)
	}
	if !strings.Contains(out, "updated sfu, sfs to 0.1.2") {
		t.Errorf("output missing update line:\n%s", out)
	}
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), string(sfuPayload))
	assertFileContents(t, filepath.Join(dir, "sfs"+exeExt()), string(sfsPayload))
}

// TestRunInstallsMissingCoreSibling is the S-07/R5 regression follow-up: a
// core sibling missing from the install dir is an ordinary pending update —
// the run installs it next to the running binary instead of leaving the
// install version-skewed behind a note line.
func TestRunInstallsMissingCoreSibling(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	newSFU, newSFL := []byte("new-sfu"), []byte("new-sfl")
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			fmt.Fprintf(w, `{"version":"0.1.2","bins":[{"name":"sfu","prefix":"SnowFastULP"},{"name":"sfs","prefix":"SnowFastSearch"},{"name":"sfl","prefix":"SnowFastLog"}],"assets":{"SnowFastULP-0.1.2-%s":{"sha256":%q,"url":%q},"SnowFastSearch-0.1.2-%s":{"sha256":%q,"url":%q},"SnowFastLog-0.1.2-%s":{"sha256":%q,"url":%q}}}`,
				suffix, hexHash(newSFU), baseURL+"/asset/sfu",
				suffix, hexHash([]byte("new-sfs")), baseURL+"/asset/sfs",
				suffix, hexHash(newSFL), baseURL+"/asset/sfl")
		case "/asset/sfu":
			_, _ = w.Write(newSFU)
		case "/asset/sfs":
			_, _ = w.Write([]byte("new-sfs"))
		case "/asset/sfl":
			_, _ = w.Write(newSFL)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	baseURL = srv.URL

	dir, hooks := installTestBinariesWithSFL(t, "old-sfu", "old-sfs", "old-sfl", "sfu")
	// The scenario: sfs was deleted from the install dir.
	if err := os.Remove(filepath.Join(dir, "sfs"+exeExt())); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: hooks.executablePath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"new tool available: sfs",
		"updated sfu, sfl to 0.1.2",
		"installed new: sfs to 0.1.2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "note:") {
		t.Errorf("output should not contain a note line:\n%s", out)
	}
	assertFileContents(t, filepath.Join(dir, "sfs"+exeExt()), "new-sfs")
	assertFileContents(t, filepath.Join(dir, "sfu"+exeExt()), string(newSFU))
	assertFileContents(t, filepath.Join(dir, "sfl"+exeExt()), string(newSFL))
}

// --- N7: timeouts ---

// TestAssetClientUsesLongerBudget proves asset downloads use the extended
// budget while manifest fetches keep the tight one.
func TestAssetClientUsesLongerBudget(t *testing.T) {
	if got := assetClient().Timeout; got != assetTimeout {
		t.Fatalf("assetClient().Timeout = %v, want %v", got, assetTimeout)
	}
	if got := httpClient().Timeout; got != httpTimeout {
		t.Fatalf("httpClient().Timeout = %v, want %v", got, httpTimeout)
	}
	if assetTimeout <= httpTimeout {
		t.Fatalf("asset budget %v must exceed manifest budget %v", assetTimeout, httpTimeout)
	}
}

// TestRunSlowAssetDownloadSucceeds proves the asset path uses the longer
// budget: with httpTimeout shrunk to 100ms and assetTimeout to 2s, an asset
// server that sleeps 400ms before writing the body sits beyond the manifest
// budget but inside the asset budget. A download still built on the
// httpTimeout client would abort at 100ms; on assetTimeout it completes and
// the payload lands. httpTimeout/assetTimeout are package vars purely so
// this discrimination is testable without multi-second waits.
func TestRunSlowAssetDownloadSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeping-asset test skipped in -short mode")
	}
	restore := shrinkTimeoutBudgets(t)
	defer restore()

	suffix := mustAssetSuffix(t)
	payload := []byte("dribbled-payload")
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			fmt.Fprintf(w, `{"version":"0.4","bins":[{"name":"sfu","prefix":"SnowFastULP"}],"assets":{"SnowFastULP-0.4-%s":{"sha256":%q,"url":"%s/asset/sfu"}}}`,
				suffix, hexHash(payload), baseURL+"/asset/sfu")
			return
		}
		time.Sleep(400 * time.Millisecond) // past httpTimeout (100ms), inside assetTimeout (2s)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	baseURL = srv.URL

	dir := t.TempDir()
	sfuPath := filepath.Join(dir, "sfu"+exeExt())
	installSnowFastFixture(t, sfuPath)

	var buf bytes.Buffer
	if err := run(nil, "0.3", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertFileContents(t, sfuPath, string(payload))
}

// shrinkTimeoutBudgets replaces the production timeout budget with tight
// test values (manifest 100ms, asset 2s) and returns a restore func. Test
// only: production init never reassigns the vars.
func shrinkTimeoutBudgets(t *testing.T) func() {
	t.Helper()
	oldHTTP, oldAsset := httpTimeout, assetTimeout
	httpTimeout = 100 * time.Millisecond
	assetTimeout = 2 * time.Second
	return func() { httpTimeout, assetTimeout = oldHTTP, oldAsset }
}

// TestRunSlowManifestFetchFailsFast is the mirror of
// TestRunSlowAssetDownloadSucceeds: a manifest handler that sleeps past the
// shrunk httpTimeout (100ms) but well inside assetTimeout (2s) must fail the
// run quickly — proving the manifest fetch keeps the tight budget instead of
// silently inheriting the asset budget.
func TestRunSlowManifestFetchFailsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeping-manifest test skipped in -short mode")
	}
	restore := shrinkTimeoutBudgets(t)
	defer restore()

	suffix := mustAssetSuffix(t)
	payload := []byte("unused-payload")
	var baseURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			// A fully valid release: if the manifest fetch ran on the
			// asset budget (2s), the run would succeed — the
			// discriminator.
			time.Sleep(400 * time.Millisecond)
			fmt.Fprintf(w, `{"version":"0.4","bins":[{"name":"sfu","prefix":"SnowFastULP"}],"assets":{"SnowFastULP-0.4-%s":{"sha256":%q,"url":"%s/asset/sfu"}}}`,
				suffix, hexHash(payload), baseURL+"/asset/sfu")
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	baseURL = srv.URL

	sfuPath := filepath.Join(t.TempDir(), "sfu"+exeExt())
	installSnowFastFixture(t, sfuPath)

	start := time.Now()
	err := run(nil, "0.3", "sfu", new(bytes.Buffer), &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: sfuPath,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("run succeeded while the manifest exceeded httpTimeout, want failure")
	}
	if !strings.Contains(err.Error(), "could not reach the update server") {
		t.Fatalf("err = %v, want update-server reachability error", err)
	}
	if elapsed >= assetTimeout {
		t.Fatalf("manifest fetch took %v (assetTimeout budget), want fail-fast on httpTimeout", elapsed)
	}
}
