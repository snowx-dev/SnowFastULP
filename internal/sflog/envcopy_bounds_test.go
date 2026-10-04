package sflog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A source larger than the cap must be rejected at the post-open Stat check:
// no destination file may be created at all.
func TestCopyFileRejectsOversizeBeforeCreate(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.env")
	if err := os.WriteFile(src, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.env")
	n, err := copyFile(src, dest, 1024)
	if !errors.Is(err, errEnvCopyOverCap) {
		t.Fatalf("err = %v, want errEnvCopyOverCap", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Fatalf("over-cap copy must not create destination; stat err=%v", serr)
	}
}

// A source exactly at the cap is allowed and the actual copied byte count is
// returned.
func TestCopyFileBoundarySizeAllowed(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("x"), 512)
	src := filepath.Join(dir, "ok.env")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "out.env")
	n, err := copyFile(src, dest, 512)
	if err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	if n != 512 {
		t.Fatalf("n = %d, want 512", n)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("dest content mismatch: %d bytes", len(got))
	}
}

// A source whose stat size fits the cap but whose readable content exceeds it
// (grew after the stat / stat reports a small size) must hit the over-copy
// branch: the partial destination is removed and errEnvCopyOverCap returned.
// Uses a procfs file, whose stat size is 0 while its content is larger, to
// reach the growth window deterministically.
func TestCopyFileGrowingSourceRemovesPartialDest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs growth probe is Linux-only")
	}
	const cap = 4
	src := "/proc/self/mountinfo"
	probe, err := os.ReadFile(src)
	if err != nil || int64(len(probe)) <= cap {
		t.Skipf("unsuitable probe file (len=%d err=%v)", len(probe), err)
	}
	if fi, serr := os.Stat(src); serr != nil || fi.Size() > cap {
		t.Skipf("unsuitable probe stat (size=%d err=%v)", fi.Size(), serr)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.env")
	// Pre-seed the destination so removal of a partial file is observable.
	if err := os.WriteFile(dest, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := copyFile(src, dest, cap)
	if !errors.Is(err, errEnvCopyOverCap) {
		t.Fatalf("err = %v, want errEnvCopyOverCap", err)
	}
	if n != cap+1 {
		t.Fatalf("n = %d, want %d (LimitReader cap+1)", n, cap+1)
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Fatalf("partial destination must be removed after over-cap copy; stat err=%v", serr)
	}
}

// A loose env file that passes EnqueueFile's pre-check but is over the cap by
// the time the worker copies it is recorded as an over-cap skip (not a write
// error), leaves no partial destination, and cleans up an empty secrets root.
func TestWriteJobOverCapAtCopyTimeSkipsWithoutPartial(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "grew.env")
	if err := os.WriteFile(src, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "sfl_stamp_secrets")
	copier := NewEnvCopier(root, nil, 512)
	var issues []EnvCopyIssue
	copier.SetErrorHandler(func(issue EnvCopyIssue) {
		issues = append(issues, issue)
	})
	copier.writeJob(envJob{srcPath: src, issuePath: src})

	es := copier.Close()
	if es.SkippedOverCap != 1 {
		t.Fatalf("SkippedOverCap = %d, want 1", es.SkippedOverCap)
	}
	if es.WriteErrors != 0 || es.WriteFailures != 0 {
		t.Fatalf("WriteErrors = %d, WriteFailures = %d, want 0/0", es.WriteErrors, es.WriteFailures)
	}
	if len(issues) != 1 || issues[0].Path != src || issues[0].Kind != EnvCopyOverCap {
		t.Fatalf("issues = %+v, want one over-cap issue for %q", issues, src)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*"))
	if len(matches) != 0 {
		t.Fatalf("partial destination left behind: %v", matches)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("empty secrets dir should be removed after over-cap skip; stat err=%v", err)
	}
}

// The tree budget passed to copyFile is the remaining tdata cap: a file that
// fits by Lstat but exceeds what is left after earlier files aborts the copy
// with errTdataOverCap and leaves no partial destination.
func TestCopyTreeRemainingBudgetReturnsTdataOverCap(t *testing.T) {
	oldCap := TdataCopyMaxBytes
	TdataCopyMaxBytes = 100
	defer func() { TdataCopyMaxBytes = oldCap }()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.bin"), make([]byte, 60), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b.bin"), make([]byte, 60), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "tdata")
	copier := NewEnvCopier(t.TempDir(), nil, EnvCopyMaxLen)
	if err := copier.CopyDir(src); !errors.Is(err, errTdataOverCap) {
		t.Fatalf("CopyDir err = %v, want errTdataOverCap", err)
	}
	if _, serr := os.Stat(dest); !os.IsNotExist(serr) {
		t.Fatalf("partial tdata destination must be wiped; stat err=%v", serr)
	}
	if copier.stats.DirsSkippedOverCap != 1 {
		t.Fatalf("DirsSkippedOverCap = %d, want 1", copier.stats.DirsSkippedOverCap)
	}
}
