package main

// Real-world test suite, gated on SFL_REALDATA_DIR pointing at the analyst data
// dir (the one holding fullz/ and raws/). Inputs are treated as strictly
// READ-ONLY: extraction/ingest tests read the real tree in place, and any -del
// test first copies its inputs into t.TempDir() and runs against the copy. Every
// library is built in t.TempDir(); the real Library/ is never touched.
//
// Run: SFL_REALDATA_DIR=/path/to/ulp go test ./cmd/sfl/ -run RealData -v
// Heavy tier (multi-GB archives, 189MB ULP): add SFL_REALDATA_HEAVY=1.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

const (
	envRealData = "SFL_REALDATA_DIR"
	envHeavy    = "SFL_REALDATA_HEAVY"
	// goodPass opens the sfl-test-fixtures encrypted archives.
	goodPass = "fullz-ice-2026"
)

// rdTime is a fixed run clock so -o/-od output filenames are deterministic.
var rdTime = time.Date(2026, 6, 27, 5, 0, 0, 0, time.UTC)

// realDataRoot returns SFL_REALDATA_DIR or skips. Also skipped under -short so
// the default `go test ./...` stays hermetic and fast.
func realDataRoot(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("real-data tests skipped under -short")
	}
	root := os.Getenv(envRealData)
	if root == "" {
		t.Skipf("set %s to the ulp data dir to run real-world tests", envRealData)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Skipf("%s=%q is not a directory", envRealData, root)
	}
	return root
}

func requireDir(t *testing.T, path string) string {
	t.Helper()
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Skipf("missing fixture dir %q (real data layout changed?)", path)
	}
	return path
}

func requireFile(t *testing.T, path string) string {
	t.Helper()
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		t.Skipf("missing fixture file %q (real data layout changed?)", path)
	}
	return path
}

func fixturesDir(t *testing.T, root string) string {
	return requireDir(t, filepath.Join(root, "fullz", "sfl-test-fixtures"))
}

// victimsParent is the folder-of-victim-folders used for loose-log shapes.
func victimsParent(t *testing.T, root string) string {
	return requireDir(t, filepath.Join(root, "fullz", "1200_130526_extracted", "1200_130526"))
}

// smallULP is the ~4MB raw ULP used for fast-tier ingest tests.
func smallULP(t *testing.T, root string) string {
	return requireFile(t, filepath.Join(root, "raws", "txt", "standart 1080 pcs 26.6.26_ulp.txt"))
}

// childDirs returns the absolute paths of dir children of parent, sorted.
func childDirs(t *testing.T, parent string) []string {
	t.Helper()
	ents, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(parent, e.Name()))
		}
	}
	sort.Strings(dirs)
	return dirs
}

// firstVictimFolder is a single real victim log folder (read-only).
func firstVictimFolder(t *testing.T, root string) string {
	dirs := childDirs(t, victimsParent(t, root))
	if len(dirs) == 0 {
		t.Skip("no victim folders under fixtures parent")
	}
	return dirs[0]
}

// copyFile copies a single regular file.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// copyTree recursively copies regular files and dirs from src to dst, skipping
// anything that is neither (symlinks, sockets) so weird stealer-log entries
// never break a -del fixture copy.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			copyFile(t, path, target)
			return nil
		default:
			return nil // skip non-regular entries
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// copyNVictims copies up to n victim subfolders from parent into input/.
func copyNVictims(t *testing.T, parent, input string, n int) int {
	t.Helper()
	dirs := childDirs(t, parent)
	if len(dirs) > n {
		dirs = dirs[:n]
	}
	for _, d := range dirs {
		copyTree(t, d, filepath.Join(input, filepath.Base(d)))
	}
	return len(dirs)
}

func loadPw(t *testing.T, value string) []string {
	t.Helper()
	pw, err := sflog.LoadPasswords(value)
	if err != nil {
		t.Fatal(err)
	}
	return pw
}

// extractResult bundles everything a real-data assertion needs from one run.
type extractResult struct {
	stats   sflog.ExtractStats
	results []sflog.SourceResult
	ulp     string
	debug   string
}

// runEngine extracts input through the shared sflog.Engine, capturing the ULP
// output and a concurrency-safe debug transcript.
func runEngine(t *testing.T, input string, passwords []string, noURI bool) extractResult {
	t.Helper()
	var mu sync.Mutex
	var dbg strings.Builder
	eng := &sflog.Engine{
		Workers:   4,
		NoURI:     noURI,
		Passwords: passwords,
		Debug: func(format string, args ...any) {
			mu.Lock()
			fmt.Fprintf(&dbg, format+"\n", args...)
			mu.Unlock()
		},
	}
	var out strings.Builder
	stats, results, err := eng.Run(context.Background(), input, &out)
	if err != nil {
		t.Fatalf("engine run on %s: %v", input, err)
	}
	return extractResult{stats: stats, results: results, ulp: out.String(), debug: dbg.String()}
}

// ulpLines returns the non-empty trimmed lines of a ULP blob.
func ulpLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// assertValidULP checks each line looks like host[:...]:user:pass.
func assertValidULP(t *testing.T, s string) {
	t.Helper()
	lines := ulpLines(s)
	if len(lines) == 0 {
		t.Fatal("expected at least one ULP line")
	}
	for _, ln := range lines {
		if strings.Count(ln, ":") < 2 {
			t.Fatalf("malformed ULP line %q (want >=2 colons)", ln)
		}
	}
}

// libLines reads every archive in a built library back to plaintext lines.
func libLines(t *testing.T, libDir string) []string {
	t.Helper()
	archives, err := filepath.Glob(filepath.Join(libDir, "sfu_*.txt.zst"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, a := range archives {
		lines = append(lines, ulpLines(readZst(t, a))...)
	}
	return lines
}

// normLibHash is a content hash of a library: decompress every archive, sort the
// lines, sha256. Order- and filename-independent so two tools that produce the
// same credential set hash identically.
func normLibHash(t *testing.T, libDir string) string {
	lines := libLines(t, libDir)
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// libUniqueCount is the number of distinct lines across the whole library.
func libUniqueCount(t *testing.T, libDir string) int64 {
	set := make(map[string]struct{})
	for _, ln := range libLines(t, libDir) {
		set[ln] = struct{}{}
	}
	return int64(len(set))
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func globOne(t *testing.T, pattern string) string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("glob %q = %v, want exactly one", pattern, matches)
	}
	return matches[0]
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- Input shapes (-o / engine) ------------------------------------------------

func TestRealDataInputSingleVictimFolder(t *testing.T) {
	root := realDataRoot(t)
	victim := firstVictimFolder(t, root)

	res := runEngine(t, victim, []string{""}, false)
	if res.stats.Emitted == 0 {
		t.Fatalf("no credentials from single victim folder %s", victim)
	}
	assertValidULP(t, res.ulp)
	if !strings.Contains(res.debug, "credentials") {
		t.Fatalf("expected debug credential events, got:\n%s", res.debug)
	}
}

func TestRealDataInputFolderOfVictims(t *testing.T) {
	root := realDataRoot(t)
	input := t.TempDir()
	n := copyNVictims(t, victimsParent(t, root), input, 4)
	if n < 2 {
		t.Skip("need >=2 victim folders for folder-of-victims shape")
	}

	// Logs counts only victim folders that held at least one recognized source,
	// so it can be <= n (some folders carry no password files).
	res := runEngine(t, input, []string{""}, false)
	if res.stats.Logs < 2 || res.stats.Logs > n {
		t.Fatalf("Logs = %d, want 2..%d (one per victim folder with sources)", res.stats.Logs, n)
	}
	if res.stats.Emitted == 0 {
		t.Fatal("no credentials from folder-of-victims")
	}
	assertValidULP(t, res.ulp)
}

func TestRealDataInputFolderOfArchives(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	input := t.TempDir()
	for _, name := range []string{"plain.zip", "encrypted.zip", "encrypted.7z"} {
		copyFile(t, filepath.Join(fx, name), filepath.Join(input, name))
	}

	// plain opens with "", encrypted open with the good password; all three hold
	// the same 2 credentials, so 6 seen dedup to 2 unique.
	res := runEngine(t, input, loadPw(t, goodPass), false)
	if res.stats.ArchivesScanned != 3 {
		t.Fatalf("ArchivesScanned = %d, want 3", res.stats.ArchivesScanned)
	}
	if res.stats.PasswordNotFound != 0 {
		t.Fatalf("PasswordNotFound = %d, want 0", res.stats.PasswordNotFound)
	}
	if res.stats.Emitted != 2 {
		t.Fatalf("Emitted = %d, want 2 unique\n%s", res.stats.Emitted, res.ulp)
	}
	if res.stats.Credentials != 6 {
		t.Fatalf("Credentials(seen) = %d, want 6", res.stats.Credentials)
	}
}

func TestRealDataInputMixedArchivesAndLoose(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	input := t.TempDir()
	copyFile(t, filepath.Join(fx, "plain.zip"), filepath.Join(input, "plain.zip"))
	copyTree(t, firstVictimFolder(t, root), filepath.Join(input, "loose-victim"))

	res := runEngine(t, input, []string{""}, false)
	if res.stats.ArchivesScanned != 1 {
		t.Fatalf("ArchivesScanned = %d, want 1", res.stats.ArchivesScanned)
	}
	if res.stats.FilesScanned == 0 {
		t.Fatal("FilesScanned = 0, want loose files from victim folder")
	}
	if res.stats.Emitted == 0 {
		t.Fatal("no credentials from mixed input")
	}
	assertValidULP(t, res.ulp)
}

// --- Passwords -----------------------------------------------------------------

func TestRealDataPasswordGoodInline(t *testing.T) {
	root := realDataRoot(t)
	enc := requireFile(t, filepath.Join(fixturesDir(t, root), "encrypted.zip"))

	res := runEngine(t, enc, loadPw(t, goodPass), false)
	if res.stats.Emitted != 2 || res.stats.PasswordNotFound != 0 {
		t.Fatalf("good inline pw: Emitted=%d PasswordNotFound=%d, want 2/0", res.stats.Emitted, res.stats.PasswordNotFound)
	}
}

func TestRealDataPasswordGoodFile(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	enc := requireFile(t, filepath.Join(fx, "encrypted.7z"))
	pwFile := requireFile(t, filepath.Join(fx, "passwords-good.txt"))

	res := runEngine(t, enc, loadPw(t, pwFile), false)
	if res.stats.Emitted != 2 || res.stats.PasswordNotFound != 0 {
		t.Fatalf("good pw file: Emitted=%d PasswordNotFound=%d, want 2/0", res.stats.Emitted, res.stats.PasswordNotFound)
	}
}

func TestRealDataPasswordBadOnlyKeepsArchive(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	enc := requireFile(t, filepath.Join(fx, "encrypted.zip"))
	pwFile := requireFile(t, filepath.Join(fx, "passwords-bad-only.txt"))

	res := runEngine(t, enc, loadPw(t, pwFile), false)
	if res.stats.Emitted != 0 {
		t.Fatalf("bad-only pw: Emitted=%d, want 0", res.stats.Emitted)
	}
	if res.stats.PasswordNotFound != 1 || res.stats.SkippedArchives != 1 {
		t.Fatalf("bad-only pw: PasswordNotFound=%d SkippedArchives=%d, want 1/1", res.stats.PasswordNotFound, res.stats.SkippedArchives)
	}
	var sawPNF bool
	for _, is := range res.stats.Issues {
		if is.Kind == sflog.IssuePasswordNotFound {
			sawPNF = true
		}
	}
	if !sawPNF {
		t.Fatalf("expected a password-not-found issue, got %+v", res.stats.Issues)
	}
	if len(res.results) != 1 || res.results[0].OK {
		t.Fatalf("results = %+v, want one not-OK archive", res.results)
	}
}

func TestRealDataPasswordMixedSucceeds(t *testing.T) {
	root := realDataRoot(t)
	fx := fixturesDir(t, root)
	enc := requireFile(t, filepath.Join(fx, "encrypted.zip"))
	pwFile := requireFile(t, filepath.Join(fx, "passwords-mixed.txt"))

	res := runEngine(t, enc, loadPw(t, pwFile), false)
	if res.stats.Emitted != 2 || res.stats.PasswordNotFound != 0 {
		t.Fatalf("mixed pw: Emitted=%d PasswordNotFound=%d, want 2/0", res.stats.Emitted, res.stats.PasswordNotFound)
	}
}

// --- Output modes (-o txt / -o -zst) ------------------------------------------

func TestRealDataOutputTxtAndZst(t *testing.T) {
	root := realDataRoot(t)
	enc := requireFile(t, filepath.Join(fixturesDir(t, root), "encrypted.zip"))

	t.Run("txt", func(t *testing.T) {
		outDir := t.TempDir()
		if err := run(runConfig{
			Input: enc, OutputDir: outDir, Password: goodPass,
			Workers: 2, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
		}); err != nil {
			t.Fatal(err)
		}
		got := readFileString(t, globOne(t, filepath.Join(outDir, "sfl_*.txt")))
		if n := len(ulpLines(got)); n != 2 {
			t.Fatalf("txt output lines = %d, want 2\n%s", n, got)
		}
		assertValidULP(t, got)
	})

	t.Run("zst", func(t *testing.T) {
		outDir := t.TempDir()
		if err := run(runConfig{
			Input: enc, OutputDir: outDir, Password: goodPass, Compress: true,
			Workers: 2, NoTUI: true, NoUpdateCheck: true, Started: rdTime,
		}); err != nil {
			t.Fatal(err)
		}
		got := readZst(t, globOne(t, filepath.Join(outDir, "sfl_*.txt.zst")))
		if n := len(ulpLines(got)); n != 2 {
			t.Fatalf("zst output lines = %d, want 2\n%s", n, got)
		}
		assertValidULP(t, got)
	})
}
