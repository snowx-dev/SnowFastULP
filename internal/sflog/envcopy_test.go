package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
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
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
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
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
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
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
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
	if err := copyFile(link, dest); err == nil {
		t.Fatal("expected error copying symlink")
	}
}

func TestWriteJobRemovesEmptyRootOnFailedCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sfl_stamp_secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	// Missing source: MkdirAll creates root, copyFile fails, empty root must go.
	copier.writeJob(envJob{srcPath: filepath.Join(t.TempDir(), "missing.env")})
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("empty secrets dir should be removed after failed write; stat err=%v", err)
	}
	if copier.stats.WriteErrors != 1 {
		t.Fatalf("WriteErrors = %d, want 1", copier.stats.WriteErrors)
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

func TestUniquePathExhaustion(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "x.env")
	if err := os.WriteFile(base, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 1000; i++ {
		p := filepath.Join(dir, "x_"+itoa(i)+".env")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := uniquePath(base); ok {
		t.Fatal("expected uniquePath exhaustion")
	}
}

func TestCopyMemberReadError(t *testing.T) {
	copier := NewEnvCopier(t.TempDir(), nil, defaultEnvCopyMaxLen)
	r := &errReader{}
	if copier.CopyMember(context.Background(), ".env", r) {
		t.Fatal("expected CopyMember to fail on read error")
	}
	if copier.stats.WriteErrors != 1 {
		t.Fatalf("WriteErrors = %d, want 1", copier.stats.WriteErrors)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

func TestArchiveEnvAlsoScannedForSecrets(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/.env")
	_, _ = w.Write([]byte(awsKeyLine + "\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	sink := &capSink{}
	e := &Engine{Workers: 1, EnvCopier: copier, SecretSink: sink, SecretMaxLen: defaultSecretMaxLen}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 1 {
		t.Fatalf("copied = %d (skipped=%d errors=%d), want 1; sink=%v", es.Copied, es.SkippedOverCap, es.WriteErrors, sink.got)
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); err != nil {
		t.Fatalf("env file not copied: %v", err)
	}
	if !sink.sawSecret(".env", "AKIA") {
		t.Fatalf("archive env member was copied but not scanned; got %v", sink.got)
	}
}

func TestCopyMemberIfCandidateReadErrorSkipsSecretScan(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	sink := &capSink{}
	prog := NewProgress()
	ec := extractCtx{
		env:          copier,
		secrets:      sink,
		secretMaxLen: defaultSecretMaxLen,
		p:            prog,
		display:      "test.rar",
	}
	if !copyMemberIfCandidate(context.Background(), ec, errReader{}, ".env", true) {
		t.Fatal("expected stream consumed")
	}
	es := copier.Close()
	if es.WriteErrors != 1 {
		t.Fatalf("WriteErrors = %d, want 1", es.WriteErrors)
	}
	if es.Copied != 0 {
		t.Fatalf("Copied = %d, want 0", es.Copied)
	}
	if len(sink.got) != 0 {
		t.Fatalf("secret sink should be empty on read error; got %v", sink.got)
	}
	if prog.SecretFilesTotal() != 0 {
		t.Fatalf("SecretFilesTotal = %d, want 0 (no credit on read error)", prog.SecretFilesTotal())
	}
}

func TestRarEnvAlsoScannedForSecrets(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "config.env", awsKeyLine+"\n")
	cmd := exec.Command(rarBin, "a", "-m0", "-ep1", "-idq", "log.rar", "config.env")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	rarPath := filepath.Join(dir, "log.rar")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	sink := &capSink{}
	e := &Engine{Workers: 1, EnvCopier: copier, SecretSink: sink, SecretMaxLen: defaultSecretMaxLen, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), rarPath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 1 {
		t.Fatalf("copied = %d (skipped=%d errors=%d), want 1; sink=%v", es.Copied, es.SkippedOverCap, es.WriteErrors, sink.got)
	}
	if _, err := os.Stat(filepath.Join(root, "config.env")); err != nil {
		t.Fatalf("env file not copied: %v", err)
	}
	if !sink.sawSecret("config.env", "AKIA") {
		t.Fatalf("rar env member was copied but not scanned; got %v", sink.got)
	}
}

func TestSevenZipEnvAlsoScannedForSecrets(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "config.env", awsKeyLine+"\n")
	cmd := exec.Command(bin, "a", "-y", "-bso0", "-bsp0", "log.7z", "config.env")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("7z create failed: %v\n%s", e, out)
	}
	path := filepath.Join(dir, "log.7z")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	sink := &capSink{}
	e := &Engine{Workers: 1, EnvCopier: copier, SecretSink: sink, SecretMaxLen: defaultSecretMaxLen, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), path, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 1 {
		t.Fatalf("copied = %d (skipped=%d errors=%d), want 1; sink=%v", es.Copied, es.SkippedOverCap, es.WriteErrors, sink.got)
	}
	if _, err := os.Stat(filepath.Join(root, "config.env")); err != nil {
		t.Fatalf("env file not copied: %v", err)
	}
	if !sink.sawSecret("config.env", "AKIA") {
		t.Fatalf("7z env member was copied but not scanned; got %v", sink.got)
	}
}

func TestSevenZipEnvSecretTotalNotDoubled(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	mustWrite(t, dir, "config.env", awsKeyLine+"\n")
	cmd := exec.Command(bin, "a", "-y", "-bso0", "-bsp0", "log.7z", "config.env")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("7z create failed: %v\n%s", e, out)
	}
	path := filepath.Join(dir, "log.7z")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	sink := &capSink{}
	prog := NewProgress()
	e := &Engine{
		Workers: 1, EnvCopier: copier, SecretSink: sink,
		SecretMaxLen: defaultSecretMaxLen, Progress: prog, Passwords: []string{""},
	}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), path, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	_ = copier.Close()
	if got := prog.SecretFilesTotal(); got != 1 {
		t.Fatalf("SecretFilesTotal = %d, want 1 (precount once; creditSecretTotal=false must not double)", got)
	}
}

func writeTestFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
