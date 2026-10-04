package selfupdate

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIsSnowFastBinaryAcceptsOwnFixture proves the identity check accepts a
// genuine binary from this module (the fixture is built from this module
// tree, so its build info carries github.com/snowx-dev/SnowFastULP).
func TestIsSnowFastBinaryAcceptsOwnFixture(t *testing.T) {
	installSnowFastFixture(t, filepath.Join(t.TempDir(), "sfu"+exeExt()))
	src, err := buildSnowFastFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	if !isSnowFastBinary(src) {
		t.Fatalf("isSnowFastBinary(%s) = false, want true for a module binary", src)
	}
	if err := verifyOverwriteTarget(src); err != nil {
		t.Fatalf("verifyOverwriteTarget(fixture) = %v, want nil", err)
	}
}

// TestVerifyOverwriteTargetRefusesPlainText covers the cheapest foreign-file
// case: a non-Go file (shell script, notes file) occupying the target name.
func TestVerifyOverwriteTargetRefusesPlainText(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sfx"+exeExt())
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho not-snowfast\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := verifyOverwriteTarget(target)
	if err == nil {
		t.Fatal("expected refusal for a text file")
	}
	var refuse *refuseOverwriteError
	if !errors.As(err, &refuse) {
		t.Fatalf("err = %T, want *refuseOverwriteError", err)
	}
	for _, want := range []string{"refusing to overwrite", target, "remove or rename it manually"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if isSnowFastBinary(target) {
		t.Fatal("isSnowFastBinary(text file) = true, want false")
	}
}

// TestVerifyOverwriteTargetRefusesForeignGoBinary builds a tiny module with a
// different module path and proves the guard refuses it — a SnowFast-named
// tool from another module must not be silently clobbered either.
func TestVerifyOverwriteTargetRefusesForeignGoBinary(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable for building a foreign module")
	}
	dir := t.TempDir()
	modDir := filepath.Join(dir, "src", "othermod")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainGo := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(modDir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/othermod\n\ngo 1.25\n"
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "foreign"+exeExt())
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = modDir
	cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(dir, "gocache"))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build foreign module: %v\n%s", err, b)
	}

	if isSnowFastBinary(out) {
		t.Fatal("isSnowFastBinary(foreign module) = true, want false")
	}
	err := verifyOverwriteTarget(out)
	if err == nil {
		t.Fatal("expected refusal for a foreign-module Go binary")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
}

// TestVerifyOverwriteTargetMissingAndDirectory covers the non-overwrite paths:
// a missing file plans a fresh install (no guard), a directory is refused.
func TestVerifyOverwriteTargetMissingAndDirectory(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent"+exeExt())
	if err := verifyOverwriteTarget(missing); err != nil {
		t.Fatalf("verifyOverwriteTarget(missing) = %v, want nil", err)
	}
	if err := verifyOverwriteTarget(dir); err == nil {
		t.Fatal("expected refusal when a directory occupies the target path")
	}
}

// TestPlanUpdatesRefusesForeignCoreBinary proves the guard protects the
// hardcoded trio too: a non-SnowFast file at the sfu target vetoes the run.
func TestPlanUpdatesRefusesForeignCoreBinary(t *testing.T) {
	suffix := mustAssetSuffix(t)
	latest := "0.4"
	dir := t.TempDir()
	ext := exeExt()
	foreign := filepath.Join(dir, "sfu"+ext)
	if err := os.WriteFile(foreign, []byte("a user's unrelated script"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := &updateManifest{
		Version: latest,
		Assets: map[string]manifestAsset{
			"SnowFastULP-" + latest + "-" + suffix: {SHA256: "00", URL: "https://example/sfu"},
		},
	}
	_, _, err := planUpdates(manifest, latest, suffix, dir, ext)
	if err == nil {
		t.Fatal("expected refusal for a foreign file at the core bin target")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
	// The foreign file is untouched.
	got, err := os.ReadFile(foreign)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a user's unrelated script" {
		t.Fatalf("foreign file clobbered: %q", got)
	}
}

// TestRunRefusesForeignExtraBin proves the full run refuses to overwrite a
// foreign sfx with a manifest extra bin, and still applies nothing.
func TestRunRefusesForeignExtraBin(t *testing.T) {
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	bins := []mockBin{
		{"sfu", "SnowFastULP", []byte("#!/bin/sh\necho sfu-0.4\n")},
		{"sfx", "SnowFastX", []byte("#!/bin/sh\necho sfx-0.4\n")},
	}
	srv := startMockReleaseServerWithBins(t, "0.4", suffix, bins)
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
	// The foreign file is untouched.
	got, err := os.ReadFile(sfxPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "precious user data" {
		t.Fatalf("sfx = %q, want untouched", got)
	}
}

// TestRunOverwriteGuardAllowsSymlinkedSelf proves the guard runs on the
// resolved target: a symlinked executable path still identifies as SnowFast
// (resolveExecutable EvalSymlinks's the invoked path, and the guard reads
// the real file behind the plan target).
func TestRunOverwriteGuardAllowsSymlinkedSelf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink self test on non-Windows")
	}
	suffix, err := assetSuffix()
	if err != nil {
		t.Skip(err)
	}
	srv, _ := startMockReleaseServer(t, "0.1.2", suffix, []byte("new-sfu"), []byte("new-sfs"))
	defer srv.Close()

	realDir := t.TempDir()
	// Real binaries in realDir; invoked through an alias symlink dir.
	realSFU := filepath.Join(realDir, "sfu"+exeExt())
	installSnowFastFixture(t, realSFU)
	installSnowFastFixture(t, filepath.Join(realDir, "sfs"+exeExt()))

	aliasDir := t.TempDir()
	aliasSFU := filepath.Join(aliasDir, "sfu"+exeExt())
	if err := os.Symlink(realSFU, aliasSFU); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	var buf bytes.Buffer
	if err := run(nil, "0.1.1", "sfu", &buf, &testHooks{
		releaseURL:     srv.URL + "/releases/latest",
		executablePath: aliasSFU,
	}); err != nil {
		t.Fatalf("run via symlinked self: %v", err)
	}
	if !strings.Contains(buf.String(), "updated sfu, sfs to 0.1.2") {
		t.Fatalf("output = %q", buf.String())
	}
	// The swap landed on the real file behind the symlink.
	assertFileContents(t, realSFU, "new-sfu")
}
