package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsEnvCopyCandidate(t *testing.T) {
	positive := []string{
		".env", ".env.local", "foo.env", "id_rsa", "secrets.json",
		"credentials.json", "wallet.dat", "config.pem", "key.key",
		"my-apikeys.json", "appsettings.Development.json",
	}
	for _, p := range positive {
		if !isEnvCopyCandidate(p) {
			t.Errorf("%q should be an env copy candidate", p)
		}
	}
	negative := []string{
		"id_rsa.pub", "id_ed25519.pub", "config", "Passwords.txt",
		"readme.md", "package.json", "image.png", "app.py", "debug.log",
	}
	for _, p := range negative {
		if isEnvCopyCandidate(p) {
			t.Errorf("%q should not be an env copy candidate", p)
		}
	}
}

func TestSafeRelPath(t *testing.T) {
	got := safeRelPath(`../evil/../../.env`)
	if got == ".." || got == "" {
		t.Fatalf("safeRelPath = %q, want sanitized path", got)
	}
	// Flat copy only keeps the basename; safeRelPath still strips ".." so the
	// basename path cannot escape the secrets root.
	if flatBasename(safeRelPath("VictimA/deep/.env")) != ".env" {
		t.Fatalf("flatBasename(safeRelPath(...)) = %q, want .env", flatBasename(safeRelPath("VictimA/deep/.env")))
	}
	if flatBasename(safeRelPath(`../evil/.env`)) != ".env" {
		t.Fatalf("traversal should still yield basename .env, got %q", flatBasename(safeRelPath(`../evil/.env`)))
	}

	root := t.TempDir()
	for _, in := range []string{`C:/tdata/key_datas`, `/tdata/key_datas`, `//share/tdata/key_datas`} {
		rel := safeRelPath(in)
		if strings.Contains(rel, ":") {
			t.Fatalf("safeRelPath(%q) leaked volume: %q", in, rel)
		}
		if filepath.IsAbs(rel) {
			t.Fatalf("safeRelPath(%q) still absolute: %q", in, rel)
		}
		dest := filepath.Join(root, rel)
		if !destUnderRoot(root, dest) {
			t.Fatalf("safeRelPath(%q)=%q joined dest escaped %q", in, rel, root)
		}
	}
}

func TestDestUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if !destUnderRoot(root, filepath.Join(root, "tdata", "key_datas")) {
		t.Fatal("child should be under root")
	}
	if destUnderRoot(root, root+"-evil") {
		t.Fatal("sibling prefix must not match")
	}
	if destUnderRoot(root, filepath.Dir(root)) {
		t.Fatal("parent must not match")
	}
}

func TestEnvCopyLooseFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "VictimA")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, victim, "config.env", "API_KEY=secret\n")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied < 1 {
		t.Fatalf("copied = %d, want >= 1", es.Copied)
	}
	if _, err := os.Stat(filepath.Join(root, "config.env")); err != nil {
		t.Fatalf("env file not copied flat: %v", err)
	}
}

func TestEnvCopyFlatArchive(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/secrets.json")
	_, _ = w.Write([]byte(`{"key":"val"}`))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 1 {
		t.Fatalf("copied = %d, want 1", es.Copied)
	}
	// Flat: the in-archive path "VictimA/secrets.json" collapses to basename.
	if _, err := os.Stat(filepath.Join(root, "secrets.json")); err != nil {
		t.Fatalf("env file not copied flat: %v", err)
	}
}

func TestEnvCopyFlatCollision(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("deep/.env")
	_, _ = w.Write([]byte("first\n"))
	w2, _ := zw.Create("other/.env")
	_, _ = w2.Write([]byte("second\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 2 {
		t.Fatalf("copied = %d, want 2", es.Copied)
	}
	if es.Deduped != 0 {
		t.Fatalf("deduped = %d, want 0 for distinct payloads", es.Deduped)
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		t.Fatalf("first .env missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".env_2")); err != nil {
		t.Fatalf("collided .env_2 missing: %v", err)
	}
}

func TestEnqueueFileSkipsOverCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.env")
	if err := os.WriteFile(path, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	copier := NewEnvCopier(t.TempDir(), nil, 512)
	copier.Start()
	copier.EnqueueFile(path)
	es := copier.Close()
	if es.SkippedOverCap != 1 {
		t.Fatalf("SkippedOverCap = %d, want 1", es.SkippedOverCap)
	}
}

func TestEnqueueFileOverCapRecordsSourceIssue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.env")
	if err := os.WriteFile(path, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	copier := NewEnvCopier(t.TempDir(), nil, 512)
	var issues []EnvCopyIssue
	copier.SetErrorHandler(func(issue EnvCopyIssue) {
		issues = append(issues, issue)
	})
	copier.Start()
	copier.EnqueueFile(path)
	es := copier.Close()
	if len(issues) != 1 || issues[0].Path != path {
		t.Fatalf("issues = %+v, want one issue for %q", issues, path)
	}
	if len(es.Issues) != 1 || es.Issues[0].Path != path {
		t.Fatalf("stats issues = %+v, want one issue for %q", es.Issues, path)
	}
	if es.WriteErrors != 0 {
		t.Fatalf("WriteErrors = %d, want 0 for a policy cap skip", es.WriteErrors)
	}
}

func TestCopyFileSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	dest := filepath.Join(dir, "out.env")
	if _, err := copyFile(link, dest, EnvCopyMaxLen); err == nil {
		t.Fatal("expected error copying symlink")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("failed copy must not create destination; stat err=%v", err)
	}
}

func TestOpenReadNoFollowRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	f, err := openReadNoFollow(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("expected error opening symlink")
	}
}

func TestWriteJobRemovesEmptyRootOnFailedCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sfl_stamp_secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	// Missing source: MkdirAll creates root, copyFile fails, empty root must go.
	copier.writeJob(envJob{srcPath: filepath.Join(t.TempDir(), "missing.env")})
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("empty secrets dir should be removed after failed write; stat err=%v", err)
	}
	if copier.stats.WriteErrors != 1 {
		t.Fatalf("WriteErrors = %d, want 1", copier.stats.WriteErrors)
	}
	if copier.stats.WriteFailures != 1 {
		t.Fatalf("WriteFailures = %d, want 1", copier.stats.WriteFailures)
	}
}

func TestRemoveRootIfEmptyKeepsNonEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kept.env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	copier := &EnvCopier{root: root}
	copier.removeRootIfEmpty()
	if _, err := os.Stat(filepath.Join(root, "kept.env")); err != nil {
		t.Fatalf("non-empty root should be kept: %v", err)
	}
}

func TestUniquePathBeyondLegacyBound(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "name.env")
	for i := range 999 {
		name := "name.env"
		if i > 0 {
			name = "name_" + itoa(i+1) + ".env"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := uniquePath(base)
	if !ok || got != filepath.Join(dir, "name_1000.env") {
		t.Fatalf("uniquePath = %q, %v; want name_1000.env, true", got, ok)
	}
}

func TestCopyMemberReadError(t *testing.T) {
	copier := NewEnvCopier(t.TempDir(), nil, EnvCopyMaxLen)
	var issues []EnvCopyIssue
	copier.SetErrorHandler(func(issue EnvCopyIssue) {
		issues = append(issues, issue)
	})
	r := &errReader{}
	if copier.CopyMember(context.Background(), "test.rar", ".env", r) {
		t.Fatal("expected CopyMember to fail on read error")
	}
	if copier.stats.WriteErrors != 1 {
		t.Fatalf("WriteErrors = %d, want 1", copier.stats.WriteErrors)
	}
	if copier.stats.ReadErrors != 1 {
		t.Fatalf("ReadErrors = %d, want 1", copier.stats.ReadErrors)
	}
	if len(issues) != 1 || issues[0].Kind != EnvCopyReadError ||
		issues[0].Path != "test.rar!.env" {
		t.Fatalf("issues = %+v, want one read error", issues)
	}
}

func TestCopyMemberReportsLateDrainError(t *testing.T) {
	copier := NewEnvCopier(t.TempDir(), nil, EnvCopyMaxLen)
	r := &prefixThenErrReader{remaining: EnvCopyMaxLen + 1}

	if copier.CopyMember(context.Background(), "test.rar", ".env", r) {
		t.Fatal("expected oversized CopyMember to fail")
	}
	if copier.stats.ReadErrors != 1 {
		t.Fatalf("ReadErrors = %d, want 1 for late drain error", copier.stats.ReadErrors)
	}
	if copier.stats.SkippedOverCap != 0 {
		t.Fatalf("SkippedOverCap = %d, want 0 when drain fails", copier.stats.SkippedOverCap)
	}
}

type prefixThenErrReader struct {
	remaining int64
}

func (r *prefixThenErrReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, os.ErrClosed
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.remaining -= int64(n)
	return n, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

func writeTestFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// constHasher is a deterministic hasher for collision tests: every payload
// hashes to the same value, forcing the byte-compare guard to decide.
type constHasher struct{ n int }

func (h *constHasher) Write(p []byte) (int, error) { h.n += len(p); return len(p), nil }
func (h *constHasher) Sum(b []byte) []byte         { return b }
func (h *constHasher) Reset()                      { h.n = 0 }
func (h *constHasher) Size() int                   { return 8 }
func (h *constHasher) BlockSize() int              { return 64 }
func (h *constHasher) Sum64() uint64               { return 42 }

// Identical loose payloads with the same basename dedup to one retained file:
// the duplicate increments Deduped and progress (not Copied), records no
// issue, and leaves no staging file behind.
func TestEnvCopyDedupsIdenticalLoosePayloads(t *testing.T) {
	dir := t.TempDir()
	dirA := filepath.Join(dir, "victimA")
	dirB := filepath.Join(dir, "victimB")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dirA, "config.env")
	b := filepath.Join(dirB, "config.env")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("API_KEY=shared\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	root := filepath.Join(t.TempDir(), "secrets")
	prog := NewProgress()
	prog.EnableEnv()
	copier := NewEnvCopier(root, prog, EnvCopyMaxLen)
	copier.Start()
	copier.EnqueueFile(a)
	copier.EnqueueFile(b)
	es := copier.Close()
	if es.Copied != 1 || es.Deduped != 1 {
		t.Fatalf("copied = %d, deduped = %d, want 1/1", es.Copied, es.Deduped)
	}
	if es.WriteErrors != 0 || len(es.Issues) != 0 {
		t.Fatalf("dedup must be issue-free: %+v", es)
	}
	if prog.EnvCopied() != 1 || prog.EnvDeduped() != 1 {
		t.Fatalf("progress copied = %d, deduped = %d, want 1/1", prog.EnvCopied(), prog.EnvDeduped())
	}
	data, err := os.ReadFile(filepath.Join(root, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "API_KEY=shared\n" {
		t.Fatalf("retained payload = %q", string(data))
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dedup must leave no staging files; entries = %v", names)
	}
}

// Identical archive members with different in-archive paths flatten to the
// same basename and dedup to one retained file.
func TestEnvCopyDedupsIdenticalArchiveMembers(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"victimA/seed.txt", "victimB/seed.txt"} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte("twelve words seed phrase\n"))
	}
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 1 || es.Deduped != 1 {
		t.Fatalf("copied = %d, deduped = %d, want 1/1", es.Copied, es.Deduped)
	}
	if _, err := os.Stat(filepath.Join(root, "seed.txt")); err != nil {
		t.Fatalf("retained seed.txt missing: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("dedup must leave no staging files; entries = %v", entries)
	}
}

// Distinct same-basename payloads are not deduped: both land as _2, _3
// suffixes and Deduped stays zero.
func TestEnvCopyKeepsDistinctSameBasenamePayloads(t *testing.T) {
	dir := t.TempDir()
	dirA := filepath.Join(dir, "victimA")
	dirB := filepath.Join(dir, "victimB")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dirA, "config.env")
	b := filepath.Join(dirB, "config.env")
	if err := os.WriteFile(a, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.writeJob(envJob{srcPath: a, issuePath: a})
	copier.writeJob(envJob{srcPath: b, issuePath: b})
	es := copier.Close()
	if es.Copied != 2 || es.Deduped != 0 {
		t.Fatalf("copied = %d, deduped = %d, want 2/0", es.Copied, es.Deduped)
	}
	first, err := os.ReadFile(filepath.Join(root, "config.env"))
	if err != nil || string(first) != "first\n" {
		t.Fatalf("config.env = %q err = %v (first content wins the basename)", first, err)
	}
	// uniquePath's existing scheme suffixes before the extension:
	// config.env -> config_2.env.
	second, err := os.ReadFile(filepath.Join(root, "config_2.env"))
	if err != nil || string(second) != "second\n" {
		t.Fatalf("config_2.env = %q err = %v", second, err)
	}
}

// A forced xxhash collision (constant test hasher) with equal sizes must NOT
// drop the second payload: the byte-compare guard keeps both files distinct.
func TestEnvCopyHashCollisionStillKeepsBothPayloads(t *testing.T) {
	prev := newEnvHasher
	newEnvHasher = func() hash.Hash64 { return &constHasher{} }
	defer func() { newEnvHasher = prev }()

	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("content-A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("content-B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.writeJob(envJob{srcPath: a, issuePath: a})
	copier.writeJob(envJob{srcPath: b, issuePath: b})
	es := copier.Close()
	if es.Copied != 2 || es.Deduped != 0 {
		t.Fatalf("copied = %d, deduped = %d, want 2/0 (collision is not a duplicate)", es.Copied, es.Deduped)
	}
	gotA, err := os.ReadFile(filepath.Join(root, "a.env"))
	if err != nil || string(gotA) != "content-A\n" {
		t.Fatalf("a.env = %q err = %v", gotA, err)
	}
	gotB, err := os.ReadFile(filepath.Join(root, "b.env"))
	if err != nil || string(gotB) != "content-B\n" {
		t.Fatalf("b.env = %q err = %v", gotB, err)
	}
}

// If the retained representative cannot be re-read, the staged payload is
// kept as a distinct output and a read error is recorded against the current
// source — never a silent loss.
func TestEnvCopyKeepsStagedWhenRepresentativeUnreadable(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.writeJob(envJob{srcPath: a, issuePath: a})
	// Remove the retained representative behind the copier's back.
	if err := os.Remove(filepath.Join(root, "a.env")); err != nil {
		t.Fatal(err)
	}
	copier.writeJob(envJob{srcPath: b, issuePath: b})
	es := copier.Close()
	if es.Copied != 2 || es.Deduped != 0 || es.ReadErrors != 1 {
		t.Fatalf("copied = %d deduped = %d readErrs = %d, want 2/0/1", es.Copied, es.Deduped, es.ReadErrors)
	}
	data, err := os.ReadFile(filepath.Join(root, "b.env"))
	if err != nil || string(data) != "same\n" {
		t.Fatalf("staged payload must survive as a distinct output: %q %v", data, err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("no staging files may remain; entries = %v", entries)
	}
}

// Telegram tdata trees are copied whole and structurally complete even when
// they contain byte-identical files: dedup never applies to directories.
func TestTdataCopyNotDeduped(t *testing.T) {
	tree1 := filepath.Join(t.TempDir(), "td1")
	tree2 := filepath.Join(t.TempDir(), "td2")
	for _, dir := range []string{tree1, tree2} {
		if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "key_datas"), []byte("keydata\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cache", "maps0"), []byte("cache-bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	if err := copier.CopyDir(tree1); err != nil {
		t.Fatal(err)
	}
	if err := copier.CopyDir(tree2); err != nil {
		t.Fatal(err)
	}
	es := copier.Close()
	if es.DirsCopied != 2 || es.Deduped != 0 || es.Copied != 0 {
		t.Fatalf("dirs = %d deduped = %d copied = %d, want 2/0/0", es.DirsCopied, es.Deduped, es.Copied)
	}
	for _, name := range []string{"tdata", "tdata_2"} {
		for _, f := range []string{"key_datas", filepath.Join("cache", "maps0")} {
			data, err := os.ReadFile(filepath.Join(root, name, f))
			if err != nil {
				t.Fatalf("%s/%s missing: %v", name, f, err)
			}
			if string(data) != "keydata\n" && string(data) != "cache-bytes\n" {
				t.Fatalf("%s/%s corrupted: %q", name, f, string(data))
			}
		}
	}
}
func TestTdataCopyBeyondLegacyBound(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tdata-src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "key_datas"), []byte("keydata\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 999 {
		name := "tdata"
		if i > 0 {
			name = "tdata_" + itoa(i+1)
		}
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	if err := copier.CopyDir(src); err != nil {
		t.Fatal(err)
	}
	if err := copier.CopyDir(src); err != nil {
		t.Fatal(err)
	}
	if copier.stats.DirsCopied != 2 {
		t.Fatalf("DirsCopied = %d, want 2", copier.stats.DirsCopied)
	}
	for _, name := range []string{"tdata_1000", "tdata_1001"} {
		if _, err := os.Stat(filepath.Join(root, name, "key_datas")); err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
}

// A real read error during the byte-compare must surface as an error, never
// be reported as a content difference (which would silently credit Copied
// and skip the promised EnvCopyReadError).
func TestReadersEqualSurfacesReadErrors(t *testing.T) {
	fa := strings.NewReader("same-bytes\n")  // (11, ErrUnexpectedEOF)
	fb := &prefixThenErrReader{remaining: 3} // (3, real read error)
	equal, err := readersEqual(fa, fb)
	if equal {
		t.Fatal("partial+error compare must not report equality")
	}
	if err == nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("err = %v, want the real read error to surface", err)
	}
}

// When the staging file of a duplicate cannot be removed, the dedup credit is
// withheld and a write error is recorded: the issue marks the source HadIssue
// so -del retains it and the stranded temp is visible.
func TestEnvCopyDedupRemoveFailureRecordsIssue(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("same\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, EnvCopyMaxLen)
	copier.writeJob(envJob{srcPath: a, issuePath: a})
	prev := osRemove
	osRemove = func(string) error { return errors.New("remove failed") }
	defer func() { osRemove = prev }()
	copier.writeJob(envJob{srcPath: b, issuePath: b})
	es := copier.Close()
	if es.Deduped != 0 || es.Copied != 1 {
		t.Fatalf("deduped = %d copied = %d, want 0/1 (credit withheld on remove failure)", es.Deduped, es.Copied)
	}
	if es.WriteErrors != 1 || es.WriteFailures != 1 {
		t.Fatalf("write errors = %d failures = %d, want 1/1", es.WriteErrors, es.WriteFailures)
	}
	if len(es.Issues) != 1 || es.Issues[0].Path != b {
		t.Fatalf("issues = %+v, want one write error for %q", es.Issues, b)
	}
	// The stranded staged temp remains (removal failed) beside the retained file.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("root entries = %v, want retained file + stranded temp", names)
	}
}
