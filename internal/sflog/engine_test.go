package sflog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestEngineConcurrentDedupAcrossFiles(t *testing.T) {
	root := t.TempDir()
	for i, body := range []string{
		"URL: a.com\nUSER: u\nPASS: p\n",
		"URL: a.com\nUSER: u\nPASS: p\n", // duplicate of file 0
		"URL: b.com\nUSER: u2\nPASS: p2\n",
	} {
		dir := filepath.Join(root, "victim", "n"+string(rune('0'+i)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	eng := &Engine{Workers: 4, Passwords: []string{""}}
	stats, results, err := eng.Run(context.Background(), root, &out)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Credentials != 3 || stats.Emitted != 2 || stats.Duplicates != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n")
	sort.Strings(got)
	want := []string{"a.com:u:p", "b.com:u2:p2"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %v want %v", got, want)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	for _, r := range results {
		if !r.OK || r.IsArchive {
			t.Fatalf("unexpected result %+v", r)
		}
	}
}

func TestEngineReportsPasswordNotFound(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "secret.zip")
	writeEncryptedTestZip(t, archivePath, "ice", "victim/Passwords.txt", "URL: https://x.com\nUSER: u\nPASS: p\n")

	var out bytes.Buffer
	eng := &Engine{Workers: 1, Passwords: []string{"", "wrong"}}
	stats, results, err := eng.Run(context.Background(), archivePath, &out)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedArchives != 1 || stats.PasswordNotFound != 1 || stats.Emitted != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(stats.Issues) != 1 || stats.Issues[0].Kind != IssuePasswordNotFound {
		t.Fatalf("issues = %+v", stats.Issues)
	}
	if len(results) != 1 || results[0].OK || !results[0].IsArchive {
		t.Fatalf("results = %+v", results)
	}
}

func TestEngineReportsNoULP(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "Passwords.txt")
	if err := os.WriteFile(p, []byte("Browser: Chrome\nProfile: Default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	eng := &Engine{Workers: 1, Passwords: []string{""}}
	stats, results, err := eng.Run(context.Background(), p, &out)
	if err != nil {
		t.Fatal(err)
	}
	if stats.NoULP != 1 || stats.Emitted != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v", results)
	}
}

func TestEngineContinuesPastUnparseableFile(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "victimA")
	bad := filepath.Join(root, "victimB")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "Passwords.txt"), []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// single token > scanner max (4MiB) makes ParseCredentials fail.
	huge := bytes.Repeat([]byte("a"), 5<<20)
	if err := os.WriteFile(filepath.Join(bad, "Passwords.txt"), huge, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	eng := &Engine{Workers: 2, Passwords: []string{""}}
	stats, _, err := eng.Run(context.Background(), root, &out)
	if err != nil {
		t.Fatalf("run should not abort: %v", err)
	}
	if stats.Emitted != 1 || stats.ParseErrors != 1 || stats.SkippedFiles != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if strings.TrimSpace(out.String()) != "a.com:u:p" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestLogGroupKeyGroupsTopLevelSubfolders(t *testing.T) {
	root := t.TempDir()
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	absRoot = filepath.Clean(absRoot)

	a1 := logGroupKey(absRoot, true, filepath.Join(root, "victimA", "Passwords.txt"))
	a2 := logGroupKey(absRoot, true, filepath.Join(root, "victimA", "deep", "Passwords.txt"))
	b := logGroupKey(absRoot, true, filepath.Join(root, "victimB", "Passwords.txt"))
	loose := logGroupKey(absRoot, true, filepath.Join(root, "loose.zip"))

	if a1 != a2 {
		t.Fatalf("files in the same top-level subfolder must share a log key: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatal("different subfolders must be different logs")
	}
	if loose == a1 || loose == b {
		t.Fatal("a loose top-level file must be its own log")
	}
	if got := logGroupKey(absRoot, false, filepath.Join(root, "whatever.txt")); got != absRoot {
		t.Fatalf("single-file input log key = %q, want root %q", got, absRoot)
	}
}

// Folder-of-folders input: each top-level victim folder is one log regardless
// of how many credential files it holds (nested included).
func TestEngineCountsLogsByTopLevelSubfolder(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("victimA", "Passwords.txt"))
	write(filepath.Join("victimA", "nested", "Passwords.txt"))
	write(filepath.Join("victimB", "Passwords.txt"))

	var out bytes.Buffer
	eng := &Engine{Workers: 3, Passwords: []string{""}}
	stats, _, err := eng.Run(context.Background(), root, &out)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Logs != 2 {
		t.Fatalf("Logs = %d, want 2 (victimA, victimB)", stats.Logs)
	}
	if stats.FilesScanned != 3 {
		t.Fatalf("FilesScanned = %d, want 3", stats.FilesScanned)
	}
}

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("disk full") }

// A failing output writer must surface the error without deadlocking, even when
// workers produce more lines than the fan-in channel can buffer.
func TestEngineWriterErrorDoesNotDeadlock(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	for i := 0; i < 6000; i++ {
		fmt.Fprintf(&sb, "URL: site%d.com\nUSER: u%d\nPASS: p%d\n\n", i, i, i)
	}
	if err := os.WriteFile(filepath.Join(root, "Passwords.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		eng := &Engine{Workers: 4, Passwords: []string{""}}
		_, _, err := eng.Run(context.Background(), root, errWriter{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected write error, got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run deadlocked on writer error")
	}
}

func TestEngineProgressReachesTotal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "victim")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Passwords.txt"), []byte("URL: a.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prog := NewProgress()
	var out bytes.Buffer
	eng := &Engine{Workers: 2, Passwords: []string{""}, Progress: prog}
	if _, _, err := eng.Run(context.Background(), root, &out); err != nil {
		t.Fatal(err)
	}
	if prog.Phase() != phaseDone {
		t.Fatalf("phase = %d", prog.Phase())
	}
	if prog.DoneBytes() != prog.Total() || prog.Fraction() != 1 {
		t.Fatalf("done=%d total=%d frac=%.3f", prog.DoneBytes(), prog.Total(), prog.Fraction())
	}
}

// The live files counter must reflect archive members, not just top-level loose
// files. Before countCredFile, an all-archive workload left prog.Files() at 0
// for the whole run (members credited only the summary stat at EOF). 10 members
// trips the parallel zip pool, so this also guards the atomic credit under -race.
func TestEngineProgressFilesCountsArchiveMembersLive(t *testing.T) {
	root := t.TempDir()
	files := make(map[string]string, 10)
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("victim%d/Passwords.txt", i)] =
			fmt.Sprintf("URL: https://s%d.example.com\nUSER: u%d\nPASS: p%d\n", i, i, i)
	}
	writeTestZip(t, filepath.Join(root, "logs.zip"), files)

	prog := NewProgress()
	var out bytes.Buffer
	eng := &Engine{Workers: 4, Passwords: []string{""}, Progress: prog}
	stats, _, err := eng.Run(context.Background(), root, &out)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesScanned == 0 {
		t.Fatal("FilesScanned = 0, want >0 (archive members not counted)")
	}
	if got := prog.Files(); got != int64(stats.FilesScanned) {
		t.Fatalf("prog.Files() = %d, want %d (live counter must match FilesScanned)", got, stats.FilesScanned)
	}
}
