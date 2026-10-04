package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
)

const (
	exitcodePartial = exitcode.Partial
	exitcodeNothing = exitcode.NothingUsable
)

// buildExitCodeBin builds the sfl binary for real-process exit-code checks.
// The run dir is the test's own t.TempDir(): cleanup is the test process's
// job, never the child's.
func buildExitCodeBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sfl")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sfl: %v: %s", err, output)
	}
	return bin
}

type exitRun struct {
	bin   string
	dir   string
	out   string
	score int
}

func newExitRun(t *testing.T) exitRun {
	t.Helper()
	dir := t.TempDir()
	return exitRun{bin: buildExitCodeBin(t), dir: dir, out: filepath.Join(dir, "out")}
}

// runSandboxed runs the built sfl with a fully sandboxed environment (fresh
// HOME/XDG/TMPDIR, empty config, no update check) and returns stderr, stdout
// and the process exit code.
func (r exitRun) runSandboxed(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	argv := []string{
		"-config", filepath.Join(r.dir, "empty.toml"),
		"-no-update-check", "-no-tui",
	}
	if r.out != "" {
		argv = append(argv, "-o", r.out+string(os.PathSeparator))
	}
	argv = append(argv, args...)
	cmd := exec.Command(r.bin, argv...)
	cmd.Dir = r.dir
	home := filepath.Join(r.dir, "home")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(r.dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(r.dir, "data"),
		"XDG_CACHE_HOME="+filepath.Join(r.dir, "cache"),
		"TMPDIR="+filepath.Join(r.dir, "tmp"),
	)
	if err := os.MkdirAll(filepath.Join(r.dir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "empty.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run sfl: %v\nstderr:\n%s", err, stderr.String())
	}
	return stderr.String(), stdout.String(), code
}

// TestExitCodePartialFailure: one failed + one successful archive exits 3,
// the failed source gets no recap row (issues log + exit code carry it), and it carries no false
// "no credential-named files found" issue (batch C3). The failed archive here is a
// valid zip with no credentials — but paired with a good archive the run is
// partial, not total.
func TestExitCodePartialFailureShowsFailedRow(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	for _, sub := range []string{"good", "empty"} {
		if err := os.MkdirAll(filepath.Join(input, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(input, "good", "Passwords.txt"), []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "empty", "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, _, code := r.runSandboxed(t, filepath.Join(r.dir, "logs"))
	if code != exitcodePartial {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodePartial, stderr)
	}
	if strings.Contains(stderr, "Unopened") {
		t.Fatalf("summary must not show an Unopened row:\n%s", stderr)
	}
	// Final frame header (2026-10-04, sfu harmonization): ✓ COMPLETE; failure
	// detail stays in the Failed row + exit code, never a warn mark.
	if !strings.Contains(stderr, "✓  COMPLETE") {
		t.Fatalf("summary must carry the ✓ COMPLETE header:\n%s", stderr)
	}
	if strings.Contains(stderr, "⚠") {
		t.Fatalf("summary must not carry a warn mark:\n%s", stderr)
	}
}

// TestExitCodeCleanRunIsZero: every source succeeds → exit 0, no Unopened row,
// and no outcome title mark.
func TestExitCodeCleanRunIsZero(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, _, code := r.runSandboxed(t, filepath.Join(r.dir, "logs"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "Failed") || strings.Contains(stderr, "Unopened") {
		t.Fatalf("clean run must not show a Failed/Unopened row:\n%s", stderr)
	}
	// Final frame header (2026-10-04, sfu harmonization): clean run carries
	// ✓ COMPLETE above the recap box.
	if !strings.Contains(stderr, "✓  COMPLETE") {
		t.Fatalf("clean run summary must carry the ✓ COMPLETE header:\n%s", stderr)
	}
}

// TestExitCodeEmptyInputIsNothingUsable: a directory with no credential-named
// files discovers nothing → exit 4 and an honest title.
func TestExitCodeEmptyInputIsNothingUsable(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "readme.txt"), []byte("no credentials here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, _, code := r.runSandboxed(t, input)
	if code != exitcodeNothing {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodeNothing, stderr)
	}
	// F3 (#13): the nothing-usable exit must name its reason in one terse
	// line after the summary (mirrors sfu), instead of exiting silently.
	if !strings.Contains(stderr, "sfl: no sources discovered in input") {
		t.Fatalf("exit-4 empty input must print the reason line:\n%s", stderr)
	}
}

// writeSalvageVolumeSet packs each named credential member into its own
// small RAR volume (rar -m0 -v4k) so the set truncates cleanly: drop the
// trailing parts and the credential member spanning the gap salvages the
// lines decoded before it. The packer's staging dir (and the loose member
// files, which would otherwise be discovered as plain sources) stay in a
// sibling directory — only the returned .partNN.rar paths exist under dir.
// Skips the test when no rar packer is installed or the packer refuses.
func writeSalvageVolumeSet(t *testing.T, dir string, members int) []string {
	t.Helper()
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found; skipping truncated-RAR exit-code test")
	}
	stage := filepath.Join(dir, "rar-staging")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := 1; i <= members; i++ {
		name := fmt.Sprintf("Passwords-%02d.txt", i)
		var b strings.Builder
		for j := 1; j <= 3; j++ {
			fmt.Fprintf(&b, "URL: https://m%02d-%d.example/login\nUSER: user%02d%d\nPASS: pass%02d%d\n", i, j, i, j, i, j)
		}
		b.WriteString(strings.Repeat("# padding to push members across volume boundaries\n", 90))
		if err := os.WriteFile(filepath.Join(stage, name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	cmd := exec.Command(rarBin, append([]string{"a", "-m0", "-v4k", "-idq", "arc.rar"}, names...)...)
	cmd.Dir = stage
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	parts, err := filepath.Glob(filepath.Join(stage, "arc.part*.rar"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(parts)
	if len(parts) < 3 {
		t.Skipf("rar produced %d part(s), need >= 3 for a truncation test", len(parts))
	}
	var kept []string
	for _, p := range parts {
		dst := filepath.Join(dir, filepath.Base(p))
		if err := os.Rename(p, dst); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, dst)
	}
	// The loose member .txt files would be discovered as plain sources and
	// dwarf the archive; the input dir gets only the volume parts.
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	return kept
}

// TestExitCodeTruncatedRarSalvageIsPartial: a truncated multi-volume RAR whose
// parts decoded usable lines before the gap salvages them to the output file —
// so the run is at worst partial (exit 3), never 4. The exit code must not
// contradict the output on disk.
func TestExitCodeTruncatedRarSalvageIsPartial(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	parts := writeSalvageVolumeSet(t, input, 12)
	for _, p := range parts[3:] { // keep part01..03: the first member salvages
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	stderr, _, code := r.runSandboxed(t, input, "-p", filepath.Join(r.dir, "no-such-passwords.txt"))
	if code != exitcodePartial {
		t.Fatalf("salvaged-truncation exit = %d, want %d (output exists on disk)\nstderr:\n%s",
			code, exitcodePartial, stderr)
	}
	if strings.Contains(stderr, "Unopened") {
		t.Fatalf("summary must not show an Unopened row (issues log carries it):\n%s", stderr)
	}
	// The salvage really wrote: the output file holds the lines parsed before
	// the gap, and the debug log (kept through this exit) names the gap.
	matches, err := filepath.Glob(filepath.Join(r.out, "sfl_*.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("salvaged output missing: %v %v\nstderr:\n%s", matches, err, stderr)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "\n"); n < 3 {
		t.Fatalf("salvaged output has %d lines, want the pre-gap credentials:\n%s", n, body)
	}
	if !strings.Contains(string(body), "m01-1.example") {
		t.Fatalf("salvaged output missing first-part credentials:\n%s", body)
	}
}

// TestExitCodeTruncatedRarNoSalvageIsNothingUsable: a truncated set with ZERO
// salvageable lines (the set's first volume is absent, so discovery sees only
// orphaned continuations) wrote nothing → still exit 4.
func TestExitCodeTruncatedRarNoSalvageIsNothingUsable(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	parts := writeSalvageVolumeSet(t, input, 12)
	// Drop the first volume: only orphaned continuations remain, no creds.
	if err := os.Remove(parts[0]); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts[3:] {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	stderr, _, code := r.runSandboxed(t, input, "-p", filepath.Join(r.dir, "no-such-passwords.txt"))
	if code != exitcodeNothing {
		t.Fatalf("zero-salvage truncation exit = %d, want %d\nstderr:\n%s", code, exitcodeNothing, stderr)
	}
	// F3 (#13): one terse reason line, matching the outcome message.
	if !strings.Contains(stderr, "sfl: all ") || !strings.Contains(stderr, "source(s) failed") {
		t.Fatalf("zero-salvage exit-4 must print the reason line:\n%s", stderr)
	}
}

// TestExitCodeTotalFailureIsDistinct: every source fails → exit 4 with the
// Failed row.
func TestExitCodeTotalFailureIsDistinct(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, _, code := r.runSandboxed(t, input)
	if code != exitcodeNothing {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodeNothing, stderr)
	}
	if strings.Contains(stderr, "Unopened") {
		t.Fatalf("total-failure summary must not show an Unopened row:\n%s", stderr)
	}
	// F3 (#13): one terse reason line, matching the outcome message.
	if !strings.Contains(stderr, "sfl: all 1 source(s) failed") {
		t.Fatalf("total-failure exit-4 must print the reason line:\n%s", stderr)
	}
}

// TestDelTransparencyRowsAndDebug: -del shows "Deleted N" and "Preserved N
// failed source(s)" rows and records the deletions in the debug log. Since
// the 2026-09-30 decision a no-ULP source records history and is deleted
// too, so both groups go and Preserved stays at 0.
func TestDelTransparencyRowsAndDebug(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	for _, sub := range []string{"good", "bad"} {
		if err := os.MkdirAll(filepath.Join(input, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(input, "good", "Passwords.txt"), []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "bad", "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, _, code := r.runSandboxed(t, filepath.Join(r.dir, "logs"), "-del", "-debug")
	if code != exitcodePartial {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodePartial, stderr)
	}
	if !strings.Contains(stderr, "Deleted") || !strings.Contains(stderr, "2") {
		t.Fatalf("summary missing Deleted row:\n%s", stderr)
	}
	if strings.Contains(stderr, "Preserved") {
		t.Fatalf("summary must not show a Preserved row when everything deleted:\n%s", stderr)
	}
	// The debug log lives next to the output dir and must record the deletion.
	matches, err := filepath.Glob(filepath.Join(r.out, "sfl_debug_*.log"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("debug log missing in output dir: %v %v", matches, err)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "del: removed 2 source unit(s), preserved 0 failed source(s)") {
		t.Fatalf("debug log missing del event:\n%s", body)
	}
}

// TestIssueLogStableLocation: the automatic issue log lands next to the
// -debug log (the -o output dir), never in TMPDIR. A source that parses to
// zero credentials (no-ULP) always records an issue.
func TestIssueLogStableLocation(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, _, code := r.runSandboxed(t, input)
	if code != exitcodeNothing {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodeNothing, stderr)
	}
	tmpLogs, err := filepath.Glob(filepath.Join(r.dir, "tmp", "sfl-issues-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpLogs) != 0 {
		t.Fatalf("issue log leaked into TMPDIR: %v", tmpLogs)
	}
	// The stable copy lives next to the -debug log (the -o output dir) and
	// the summary footer points at it.
	matches, _ := filepath.Glob(filepath.Join(r.out, "sfl-issues-*.log"))
	if len(matches) == 0 {
		t.Fatalf("no issue log in the stable output dir\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, filepath.Base(matches[0])) {
		t.Fatalf("summary footer missing issue log path %s:\n%s", matches[0], stderr)
	}
}

// TestIssueLogODRDryRunStaysOutOfLibrary: -odr is a read-only history
// consult — previewing a library must not leave the automatic issue log
// inside it. The log takes the CWD fallback instead (the documented chain's
// last stable stop; TMPDIR stays a last-resort fallback only).
func TestIssueLogODRDryRunStaysOutOfLibrary(t *testing.T) {
	r := newExitRun(t)
	lib := filepath.Join(r.dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(lib)
	if err != nil {
		t.Fatal(err)
	}
	// A no-ULP input always records an issue, so the lazy issue log opens.
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// CWD is r.dir; the library is a subdir of it, so an in-library log would
	// also match a naive CWD glob — assert both sides explicitly. The harness
	// defaults to -o r.out; -o and -od/-odr are mutually exclusive, so drop it.
	r.out = ""
	stderr, _, code := r.runSandboxed(t, input, "-odr", lib)
	if code != exitcodeNothing {
		t.Fatalf("-odr no-ULP exit = %d, want %d\nstderr:\n%s", code, exitcodeNothing, stderr)
	}
	after, err := os.ReadDir(lib)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		names := func(entries []os.DirEntry) []string {
			var out []string
			for _, e := range entries {
				out = append(out, e.Name())
			}
			return out
		}
		t.Fatalf("dry-run consult left files in the library: before=%v after=%v", names(before), names(after))
	}
	inLib, err := filepath.Glob(filepath.Join(lib, "sfl-issues-*.log"))
	if err != nil || len(inLib) != 0 {
		t.Fatalf("issue log leaked into the consulted library: %v %v", inLib, err)
	}
	inCWD, err := filepath.Glob(filepath.Join(r.dir, "sfl-issues-*.log"))
	if err != nil || len(inCWD) != 1 {
		t.Fatalf("issue log missing in CWD fallback: %v %v\nstderr:\n%s", inCWD, err, stderr)
	}
	if !strings.Contains(stderr, filepath.Base(inCWD[0])) {
		t.Fatalf("summary footer missing issue log path %s:\n%s", inCWD[0], stderr)
	}
}

// TestIssueLogRealIngestStillUsesLibraryDir: the placement seam's untouched
// side — a real -od run (no dry run) still lands the issue log in the library
// dir next to the -debug log, per the documented chain.
func TestIssueLogRealIngestStillUsesLibraryDir(t *testing.T) {
	r := newExitRun(t)
	lib := filepath.Join(r.dir, "library")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Drop the harness's default -o (mutually exclusive with -od).
	r.out = ""
	stderr, _, code := r.runSandboxed(t, input, "-od", lib)
	if code != exitcodeNothing {
		t.Fatalf("-od no-ULP exit = %d, want %d\nstderr:\n%s", code, exitcodeNothing, stderr)
	}
	inLib, err := filepath.Glob(filepath.Join(lib, "sfl-issues-*.log"))
	if err != nil || len(inLib) != 1 {
		t.Fatalf("real -od run must keep the issue log in the library dir: %v %v\nstderr:\n%s", inLib, err, stderr)
	}
}

// TestIssueLogStableLocationNoOutputDir: the default invocation (no -o/-od)
// must put the issue log next to the -debug log in CWD, not in TMPDIR.
func TestIssueLogStableLocationNoOutputDir(t *testing.T) {
	r := newExitRun(t)
	input := filepath.Join(r.dir, "logs")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("Browser: Chrome\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No -o: run() defaults the output dir to "."; both logs must land in
	// the process CWD (r.dir) together.
	r.out = ""
	stderr, _, code := r.runSandboxed(t, input, "-debug")
	if code != exitcodeNothing {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, exitcodeNothing, stderr)
	}
	tmpLogs, err := filepath.Glob(filepath.Join(r.dir, "tmp", "sfl-issues-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpLogs) != 0 {
		t.Fatalf("issue log leaked into TMPDIR: %v", tmpLogs)
	}
	debugLogs, err := filepath.Glob(filepath.Join(r.dir, "sfl_debug_*.log"))
	if err != nil || len(debugLogs) != 1 {
		t.Fatalf("debug log missing in CWD: %v %v", debugLogs, err)
	}
	issueLogs, err := filepath.Glob(filepath.Join(r.dir, "sfl-issues-*.log"))
	if err != nil || len(issueLogs) != 1 {
		t.Fatalf("issue log missing in CWD: %v %v\nstderr:\n%s", issueLogs, err, stderr)
	}
}
