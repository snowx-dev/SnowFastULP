package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

// A run with zero emitted issues must close to a zero result and create no
// file anywhere in the platform temp root.
func TestIssueLoggerZeroIssuesCreatesNoFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	defer func() {
		// A zero-issue run must not write anywhere (TMPDIR or the stable dir).
		entries, _ := os.ReadDir(tmp)
		if len(entries) != 0 {
			t.Errorf("stable dir polluted: %v", entries)
		}
	}()
	l := newIssueLogger(runConfig{RunStamp: "20260920_test05", OutputDir: tmp})
	res := l.Close()
	if res.Path != "" || res.Count != 0 || res.Err != nil {
		t.Fatalf("zero-issue close = %+v, want zero result", res)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("clean run must not create an issue file; entries = %v", names)
	}
}

// The lazily created log lands in the stable location (cfg.OutputDir — the
// same place the -debug log goes), is named after the run stamp, and holds
// the header, one TSV line per issue, and the total footer. Nothing may land
// in the platform temp root.
func TestIssueLoggerStablePlacementAndTSVContent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	cfg := runConfig{RunStamp: "20260920_test06", OutputDir: tmp}
	l := newIssueLogger(cfg)
	l.Record("/data/locked.zip", sflog.IssuePasswordNotFound, nil)
	l.Record("/data/mixed.txt", sflog.IssueMixedFormat, nil)
	l.Record("/data/corrupt.zip", sflog.IssueParseError, errors.New("boom"))
	res := l.Close()
	if res.Err != nil {
		t.Fatalf("close err: %v", res.Err)
	}
	if res.Count != 3 {
		t.Fatalf("count = %d, want 3", res.Count)
	}
	matches, err := filepath.Glob(filepath.Join(tmp, "sfl-issues-20260920_test06-*.log"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("matches = %v err = %v, want one issue log in the stable dir", matches, err)
	}
	if res.Path != matches[0] {
		t.Fatalf("close path = %q, want %q", res.Path, matches[0])
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := []string{
		"# sfl issues — " + startedOrNow(cfg).Format("2006-01-02 15:04:05"),
		"# kind\tpath\tdetail",
		"password-not-found\t/data/locked.zip\tnone of the candidate passwords worked",
		"mixed-format\t/data/mixed.txt\tlabeled blocks kept; valid label-less URL/login/password lines discarded",
		"parse-error\t/data/corrupt.zip\tboom",
		"# 3 issue(s) total",
	}
	if len(got) != len(want) {
		t.Fatalf("issue log lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDebugMixedFormatIssueWordingUnchanged(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "debug-*.log")
	if err != nil {
		t.Fatal(err)
	}
	d := &debugLogger{f: f}
	d.Issues(sflog.ExtractStats{Issues: []sflog.Issue{{Path: "/data/mixed.txt", Kind: sflog.IssueMixedFormat}}})
	d.Close()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `mixed-format path="/data/mixed.txt"`) || strings.Contains(got, "valid label-less URL/login/password lines discarded") {
		t.Fatalf("mixed-format debug output changed: %q", got)
	}
}

// The temp file is mode 0600 where permissions are supported (Unix).
func TestIssueLoggerFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permissions are not enforced on windows")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	l := newIssueLogger(runConfig{RunStamp: "20260920_test07", OutputDir: tmp})
	l.Record("/a.zip", sflog.IssueOpenError, errors.New("gone"))
	res := l.Close()
	if res.Err != nil {
		t.Fatalf("close err: %v", res.Err)
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("issue log mode = %v, want 0600", info.Mode().Perm())
	}
}

// Record is safe under concurrent workers and stays uncapped: every event is
// counted and written.
func TestIssueLoggerConcurrentUncappedWrites(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	l := newIssueLogger(runConfig{RunStamp: "20260920_test08", OutputDir: tmp})
	const goroutines, per = 8, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				l.Record(fmt.Sprintf("/data/g%d-%d.zip", g, i), sflog.IssueOpenError, errors.New("gone"))
			}
		}(g)
	}
	wg.Wait()
	res := l.Close()
	if res.Count != goroutines*per {
		t.Fatalf("count = %d, want %d (uncapped)", res.Count, goroutines*per)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for g := 0; g < goroutines; g++ {
		for i := 0; i < per; i++ {
			if !strings.Contains(body, fmt.Sprintf("/data/g%d-%d.zip", g, i)) {
				t.Fatalf("issue log missing /data/g%d-%d.zip", g, i)
			}
		}
	}
	if n := strings.Count(body, "\n"); n != goroutines*per+3 { // 2 header lines + footer
		t.Fatalf("issue log has %d lines, want %d", n, goroutines*per+3)
	}
}

// Close is idempotent: explicit and deferred closes return the same stored
// result.
func TestIssueLoggerCloseIdempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	l := newIssueLogger(runConfig{RunStamp: "20260920_test09", OutputDir: tmp})
	l.Record("/a.zip", sflog.IssueOpenError, errors.New("gone"))
	first := l.Close()
	second := l.Close()
	if first != second {
		t.Fatalf("first close = %+v, second = %+v", first, second)
	}
}

// A create failure in the stable dir AND in the platform temp fallback is
// stored once, never retries, keeps counting incoming events, and reports
// success-free semantics (empty Path, sticky Err) while the run itself still
// completes. The stable dir is blocked by a regular file so MkdirAll fails;
// the fallback seam then fails too.
func TestIssueLoggerCreateFailureIsSticky(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	boom := errors.New("no space left on device")
	prev := createIssueTemp
	createIssueTemp = func(string) (*os.File, error) { return nil, boom }
	defer func() { createIssueTemp = prev }()

	blocker := filepath.Join(dir, "blocked-output")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newIssueLogger(runConfig{RunStamp: "20260920_test10", OutputDir: blocker})
	l.Record("/a.zip", sflog.IssueOpenError, errors.New("gone"))
	l.Record("/b.zip", sflog.IssueOpenError, errors.New("gone"))
	res := l.Close()
	if res.Count != 2 || res.Path != "" || !errors.Is(res.Err, boom) {
		t.Fatalf("close = %+v, want count 2, empty path, sticky create error", res)
	}
	if again := l.Close(); again != res {
		t.Fatalf("deferred close = %+v, want %+v", again, res)
	}
	if line := issueFailureLine(res); !strings.Contains(line, "could not write issue details") || !strings.Contains(line, "no space left") {
		t.Fatalf("failure line = %q, want plain could-not-write diagnostic", line)
	}
	if issueFooterBlock(res) != nil {
		t.Fatal("failed close must not produce an Issues footer")
	}
}

// A write failure after successful creation is sticky too: later records keep
// counting, the close fails, and no footer is offered.
func TestIssueLoggerWriteFailureIsSticky(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.log")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	ro, err := os.Open(path) // opened read-only so writes fail
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close() //nolint:errcheck // test cleanup
	prev := createIssueTemp
	createIssueTemp = func(string) (*os.File, error) { return ro, nil }
	defer func() { createIssueTemp = prev }()

	// Block the stable dir so the fallback seam (read-only file) is used.
	blocker := filepath.Join(dir, "blocked-output")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newIssueLogger(runConfig{RunStamp: "20260920_test11", OutputDir: blocker})
	l.Record("/a.zip", sflog.IssueOpenError, errors.New("gone"))
	l.Record("/b.zip", sflog.IssueOpenError, errors.New("gone"))
	res := l.Close()
	if res.Count != 2 || res.Path != "" || res.Err == nil {
		t.Fatalf("close = %+v, want count 2, empty path, sticky write error", res)
	}
	if issueFooterBlock(res) != nil {
		t.Fatal("failed close must not produce an Issues footer")
	}
}

// A successful close with issues produces exactly the muted Issues footer.
func TestIssueFooterBlockPresenceAndAbsence(t *testing.T) {
	ok := issueLogResult{Path: "/tmp/sfl-issues-20260920_test12-1.log", Count: 3}
	block := issueFooterBlock(ok)
	if block == nil {
		t.Fatal("successful close with issues must produce an Issues footer")
	}
	joined := strings.Join(block, "\n")
	if !strings.Contains(joined, "/tmp/sfl-issues-20260920_test12-1.log") {
		t.Fatalf("footer missing temp log path:\n%s", joined)
	}
	if !strings.Contains(joined, "Issues") {
		t.Fatalf("footer missing Issues label:\n%s", joined)
	}
	if got := issueFooterBlock(issueLogResult{Count: 0}); got != nil {
		t.Fatalf("zero issues must produce no footer, got %v", got)
	}
}

// captureSflStderr redirects os.Stderr (the var reportIssueLog prints to) for
// the duration of fn and returns what was written.
func captureSflStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Early exits that skip the summary must still surface the automatic issue
// log: the muted path footer on a clean close with issues, the plain
// diagnostic on a failed close, and nothing at all for a zero-issue log.
func TestReportIssueLogOnEarlyExits(t *testing.T) {
	ok := issueLogResult{Path: "/tmp/sfl-issues-20260920_fix-1.log", Count: 2}
	out := captureSflStderr(t, func() { reportIssueLog(ok) })
	if !strings.Contains(out, "/tmp/sfl-issues-20260920_fix-1.log") || !strings.Contains(out, "Issues") {
		t.Fatalf("early exit must surface the issue log path:\n%s", out)
	}

	fail := issueLogResult{Path: "", Count: 2, Err: errors.New("no space left on device")}
	out = captureSflStderr(t, func() { reportIssueLog(fail) })
	if !strings.Contains(out, "could not write issue details") || !strings.Contains(out, "no space left") {
		t.Fatalf("failed close must print the diagnostic:\n%s", out)
	}
	if strings.Contains(out, "Issues") {
		t.Fatalf("failed close must not print an Issues footer:\n%s", out)
	}

	out = captureSflStderr(t, func() { reportIssueLog(issueLogResult{}) })
	if out != "" {
		t.Fatalf("zero-issue close must print nothing, got:\n%s", out)
	}
}
