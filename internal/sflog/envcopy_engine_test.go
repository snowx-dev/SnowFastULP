package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooseEnvCopiedPerLog(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "VictimA")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, victim, "config.env", "API_KEY=secret\n")
	writeTestFile(t, victim, "Information.txt", "PC: test-pc\n")

	root := filepath.Join(t.TempDir(), "env", "202601011200")
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
	if es.ContextCopied < 1 {
		t.Fatalf("context copied = %d, want >= 1", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "VictimA", "config.env")); err != nil {
		t.Fatalf("env file not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "VictimA", "information.txt")); err != nil {
		t.Fatalf("context file not copied: %v", err)
	}
}

func TestArchiveEnvCopiedWithContext(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("secrets.json")
	_, _ = w.Write([]byte(`{"key":"val"}`))
	w2, _ := zw.Create("Information.txt")
	_, _ = w2.Write([]byte("user: alice\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied < 1 {
		t.Fatalf("copied = %d, want >= 1", es.Copied)
	}
	if es.ContextCopied < 1 {
		t.Fatalf("context = %d, want >= 1", es.ContextCopied)
	}
}

func TestMegaZipContextGatedPerVictimPrefix(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "mega.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/profile.ovpn")
	_, _ = w.Write([]byte("vpn-data\n"))
	w2, _ := zw.Create("VictimA/information.txt")
	_, _ = w2.Write([]byte("pc-a\n"))
	w3, _ := zw.Create("VictimB/information.txt")
	_, _ = w3.Write([]byte("pc-b\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 1 {
		t.Fatalf("context = %d, want 1 (VictimA only)", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "mega.zip", "VictimA", "information.txt")); err != nil {
		t.Fatalf("VictimA context missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "mega.zip", "VictimB", "information.txt")); !os.IsNotExist(err) {
		t.Fatalf("VictimB context should not be copied")
	}
}

func TestBundleZipContextGatedPerVictimKey(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Logs_1 July/VE[abc]/Vpns/tunnel.ovpn")
	_, _ = w.Write([]byte("vpn-data\n"))
	w2, _ := zw.Create("Logs_1 July/VE[abc]/information.txt")
	_, _ = w2.Write([]byte("pc-a\n"))
	w3, _ := zw.Create("Logs_1 July/IN[def]/information.txt")
	_, _ = w3.Write([]byte("pc-b\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 1 {
		t.Fatalf("context = %d, want 1 (VE[abc] only)", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "Logs_1 July", "VE[abc]", "information.txt")); err != nil {
		t.Fatalf("VE context missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "Logs_1 July", "IN[def]", "information.txt")); !os.IsNotExist(err) {
		t.Fatalf("IN context should not be copied without env")
	}
}

func TestTripleNestingBundleContextGated(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("Batch/Logs_1 July/VE[abc]/Vpns/tunnel.ovpn")
	_, _ = w.Write([]byte("vpn-data\n"))
	w2, _ := zw.Create("Batch/Logs_1 July/VE[abc]/information.txt")
	_, _ = w2.Write([]byte("pc-a\n"))
	w3, _ := zw.Create("Batch/Logs_1 July/IN[def]/information.txt")
	_, _ = w3.Write([]byte("pc-b\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 1 {
		t.Fatalf("context = %d, want 1", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "Batch", "Logs_1 July", "VE[abc]", "information.txt")); err != nil {
		t.Fatalf("VE context missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "Batch", "Logs_1 July", "IN[def]", "information.txt")); !os.IsNotExist(err) {
		t.Fatalf("IN context should not be copied without env")
	}
}

func TestSubfolderContextCopiedWithEnv(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/profile.ovpn")
	_, _ = w.Write([]byte("vpn-data\n"))
	w2, _ := zw.Create("VictimA/information.txt")
	_, _ = w2.Write([]byte("pc-a\n"))
	w3, _ := zw.Create("VictimA/DungeonCheckerGmail/info.txt")
	_, _ = w3.Write([]byte("gmail-check\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 2 {
		t.Fatalf("context = %d, want 2", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "VictimA", "info.txt")); err != nil {
		t.Fatalf("subfolder context missing: %v", err)
	}
}

func TestArchiveEnvOnlyNoContext(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "envonly.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/profile.ovpn")
	_, _ = w.Write([]byte("vpn-data\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 0 {
		t.Fatalf("context = %d, want 0", es.ContextCopied)
	}
}

func TestRootEnvDoesNotCopyVictimContexts(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "rootenv.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("secrets.json")
	_, _ = w.Write([]byte(`{"key":"val"}`))
	w2, _ := zw.Create("VictimA/information.txt")
	_, _ = w2.Write([]byte("pc-a\n"))
	w3, _ := zw.Create("VictimB/information.txt")
	_, _ = w3.Write([]byte("pc-b\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 0 {
		t.Fatalf("context = %d, want 0 (no root Information.txt)", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "rootenv.zip", "information.txt")); !os.IsNotExist(err) {
		t.Fatalf("VictimA context should not be copied")
	}
}

func TestZipInZipContextGatedPerArchive(t *testing.T) {
	dir := t.TempDir()
	var innerBuf bytes.Buffer
	izw := zip.NewWriter(&innerBuf)
	w, _ := izw.Create("VictimA/secrets.json")
	_, _ = w.Write([]byte(`{"key":"val"}`))
	_ = izw.Close()

	archivePath := filepath.Join(dir, "outer.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w2, _ := zw.Create("inner.zip")
	_, _ = w2.Write(innerBuf.Bytes())
	w3, _ := zw.Create("VictimA/information.txt")
	_, _ = w3.Write([]byte("pc-a\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 0 {
		t.Fatalf("context = %d, want 0 (outer context must not pair with inner env)", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "outer.zip", "VictimA", "secrets.json")); err != nil {
		t.Fatalf("inner env missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "outer.zip", "VictimA", "information.txt")); !os.IsNotExist(err) {
		t.Fatalf("outer context should not be copied")
	}
	index, err := os.ReadFile(filepath.Join(root, "outer.zip", "VictimA", "index.txt"))
	if err != nil {
		t.Fatalf("index missing: %v", err)
	}
	if !strings.Contains(string(index), "secrets.json\tinner.zip/VictimA/secrets.json") {
		t.Fatalf("index missing nested provenance: %q", index)
	}
}

func TestSubfolderOnlyContext(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/profile.ovpn")
	_, _ = w.Write([]byte("vpn-data\n"))
	w2, _ := zw.Create("VictimA/DungeonCheckerGmail/info.txt")
	_, _ = w2.Write([]byte("gmail-check\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if es.ContextCopied != 1 {
		t.Fatalf("context = %d, want 1", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "VictimA", "info.txt")); err != nil {
		t.Fatalf("subfolder context missing: %v", err)
	}
}

func TestLooseBatchContextGated(t *testing.T) {
	dir := t.TempDir()
	batch := filepath.Join(dir, "Batch")
	victimA := filepath.Join(batch, "VictimA")
	victimB := filepath.Join(batch, "VictimB")
	for _, d := range []string{victimA, victimB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, victimA, "config.env", "KEY=secret\n")
	writeTestFile(t, victimA, "Information.txt", "PC: a\n")
	writeTestFile(t, victimB, "Information.txt", "PC: b\n")

	root := filepath.Join(t.TempDir(), "env", "stamp")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 1 {
		t.Fatalf("copied = %d, want 1", es.Copied)
	}
	if es.ContextCopied != 1 {
		t.Fatalf("context = %d, want 1 (VictimA only)", es.ContextCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "Batch", "VictimA", "information.txt")); err != nil {
		t.Fatalf("VictimA context missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Batch", "VictimB", "information.txt")); !os.IsNotExist(err) {
		t.Fatalf("VictimB context should not be copied")
	}
}

func TestContextNotCopiedWithoutEnv(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "VictimB")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, victim, "Information.txt", "PC: lonely\n")

	root := filepath.Join(t.TempDir(), "env", "202601011200")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.Copied != 0 || es.ContextCopied != 0 {
		t.Fatalf("expected no copies, got copied=%d context=%d", es.Copied, es.ContextCopied)
	}
}

func TestEnvCopyWritesIndex(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "mega.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("VictimA/.env")
	_, _ = w.Write([]byte("A=1\n"))
	w2, _ := zw.Create("VictimB/.env")
	_, _ = w2.Write([]byte("B=2\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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

	indexA, err := os.ReadFile(filepath.Join(root, "mega.zip", "VictimA", "index.txt"))
	if err != nil {
		t.Fatalf("VictimA index missing: %v", err)
	}
	if !strings.Contains(string(indexA), ".env\tVictimA/.env") {
		t.Fatalf("VictimA index missing env line:\n%s", indexA)
	}
	indexB, err := os.ReadFile(filepath.Join(root, "mega.zip", "VictimB", "index.txt"))
	if err != nil {
		t.Fatalf("VictimB index missing: %v", err)
	}
	if !strings.Contains(string(indexB), ".env\tVictimB/.env") {
		t.Fatalf("VictimB index missing env line:\n%s", indexB)
	}
}

func TestEnvCopyFlatCollision(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bundle.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("deep/nested/.env")
	_, _ = w.Write([]byte("nested=1\n"))
	w2, _ := zw.Create("other/.env")
	_, _ = w2.Write([]byte("other=2\n"))
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "env", "stamp")
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
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "deep", ".env")); err != nil {
		t.Fatalf(".env missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bundle.zip", "other", ".env")); err != nil {
		t.Fatalf("other .env missing: %v", err)
	}
	indexDeep, err := os.ReadFile(filepath.Join(root, "bundle.zip", "deep", "index.txt"))
	if err != nil {
		t.Fatalf("deep index missing: %v", err)
	}
	if !strings.Contains(string(indexDeep), ".env\tdeep/nested/.env") {
		t.Fatalf("unexpected deep index:\n%s", indexDeep)
	}
	indexOther, err := os.ReadFile(filepath.Join(root, "bundle.zip", "other", "index.txt"))
	if err != nil {
		t.Fatalf("other index missing: %v", err)
	}
	if !strings.Contains(string(indexOther), ".env\tother/.env") {
		t.Fatalf("unexpected other index:\n%s", indexOther)
	}
}

func writeTestFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
