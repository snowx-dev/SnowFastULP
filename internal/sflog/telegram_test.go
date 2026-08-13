package sflog

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsKeyDataFile(t *testing.T) {
	accept := []string{"key_datas", "key_data#2s", "key_data#3s", "key_data#10s"}
	for _, n := range accept {
		if !isKeyDataFile(n) {
			t.Errorf("isKeyDataFile(%q) = false, want true", n)
		}
	}
	reject := []string{"key_data", "key_dataa", "key_data#", "key_data#s", "Key_datas",
		"key_data#2", "key_datas.bak", "settings", "maps0", "usertag"}
	for _, n := range reject {
		if isKeyDataFile(n) {
			t.Errorf("isKeyDataFile(%q) = true, want false", n)
		}
	}
}

func TestTdataMemberPrefix(t *testing.T) {
	cases := []struct {
		name        string
		wantPrefix  string
		wantRel     string
		wantOK      bool
	}{
		{"tdata/key_datas", "tdata", "key_datas", true},
		{"VictimA/tdata/key_datas", "VictimA/tdata", "key_datas", true},
		{"VictimA/tdata/D877F783D5D3EF8C/maps0", "VictimA/tdata", "D877F783D5D3EF8C/maps0", true},
		{"a/b/tdata/c/key_data#2s", "a/b/tdata", "c/key_data#2s", true},
		{`VictimA\tdata\key_datas`, "VictimA/tdata", "key_datas", true}, // backslashes normalized
		{"notdata/key_datas", "", "", false},
		{"tdata_backup/key_datas", "", "", false},
		{"key_datas", "", "", false}, // no tdata segment
	}
	for _, c := range cases {
		prefix, rel, ok := tdataMemberPrefix(c.name)
		if ok != c.wantOK || prefix != c.wantPrefix || rel != c.wantRel {
			t.Errorf("tdataMemberPrefix(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.name, prefix, rel, ok, c.wantPrefix, c.wantRel, c.wantOK)
		}
	}
}

func TestIsTelegramTdataDir(t *testing.T) {
	dir := t.TempDir()
	// Real tdata: dir named tdata with key_datas.
	td := filepath.Join(dir, "Telegram Desktop", "tdata")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(td, "D877F783D5D3EF8C"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "D877F783D5D3EF8C", "maps0"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isTelegramTdataDir(td) {
		t.Errorf("isTelegramTdataDir(%q) = false, want true", td)
	}
	// Multi-account variant: key_data#2s only.
	td2 := filepath.Join(dir, "alt", "tdata")
	if err := os.MkdirAll(td2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td2, "key_data#2s"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isTelegramTdataDir(td2) {
		t.Errorf("isTelegramTdataDir(%q) = false, want true (multi-account key_data#2s)", td2)
	}
	// Decoy: tdata without key_data*.
	dec := filepath.Join(dir, "decoy", "tdata")
	if err := os.MkdirAll(dec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dec, "maps0"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isTelegramTdataDir(dec) {
		t.Errorf("isTelegramTdataDir(%q) = true, want false (no key_data*)", dec)
	}
	// Not named tdata at all.
	if isTelegramTdataDir(filepath.Join(dir, "Telegram Desktop")) {
		t.Errorf("isTelegramTdataDir on non-tdata dir = true, want false")
	}
	// Symlink named key_datas must not confirm the tree.
	td3 := filepath.Join(dir, "sym", "tdata")
	if err := os.MkdirAll(td3, 0o755); err != nil {
		t.Fatal(err)
	}
	dummy := filepath.Join(dir, "dummy-target")
	if err := os.WriteFile(dummy, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dummy, filepath.Join(td3, "key_datas")); err == nil {
		if isTelegramTdataDir(td3) {
			t.Errorf("symlink key_datas should not confirm a tdata dir")
		}
	}
}

func TestEnvCopyLooseTdataDir(t *testing.T) {
	dir := t.TempDir()
	// Build a realistic-ish tdata tree under a victim folder.
	td := filepath.Join(dir, "VictimA", "Telegram Desktop", "tdata")
	if err := os.MkdirAll(filepath.Join(td, "D877F783D5D3EF8C"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("localkey"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "settingss"), []byte("settings"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "D877F783D5D3EF8C", "maps0"), []byte("maps"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, ".env"), []byte("API=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlinked file inside tdata must be skipped, not followed.
	linkTarget := filepath.Join(dir, "link-target")
	if err := os.WriteFile(linkTarget, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	hasLink := os.Symlink(linkTarget, filepath.Join(td, "link")) == nil

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (copied=%d errors=%d)", es.DirsCopied, es.Copied, es.WriteErrors)
	}
	if es.Copied != 4 {
		t.Fatalf("Copied = %d, want 4 regular files", es.Copied)
	}
	// The whole tree lands under <secrets>/tdata/ preserving structure.
	dst := filepath.Join(root, "tdata")
	if _, err := os.Stat(filepath.Join(dst, "key_datas")); err != nil {
		t.Fatalf("key_datas not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "settingss")); err != nil {
		t.Fatalf("settingss not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "D877F783D5D3EF8C", "maps0")); err != nil {
		t.Fatalf("nested maps0 not copied: %v", err)
	}
	// Symlink must NOT be followed/copied.
	if hasLink {
		if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
			t.Fatalf("symlink should not be copied: stat err=%v", err)
		}
	}
	// Interior files must NOT also be flat-copied (SkipDir prevents the flat walk).
	if _, err := os.Stat(filepath.Join(root, "key_datas")); !os.IsNotExist(err) {
		t.Fatalf("key_datas should not be flat-copied at secrets root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env inside tdata should not be flat-copied at secrets root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".env")); err != nil {
		t.Fatalf(".env should be copied inside tdata tree: %v", err)
	}
}

func TestEnvCopyTdataDirCollision(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"VictimA", "VictimB"} {
		td := filepath.Join(dir, v, "tdata")
		if err := os.MkdirAll(td, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 2 {
		t.Fatalf("DirsCopied = %d, want 2", es.DirsCopied)
	}
	// First -> tdata, second -> tdata_2.
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_datas")); err != nil {
		t.Fatalf("first tdata missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata_2", "key_datas")); err != nil {
		t.Fatalf("collided tdata_2 missing: %v", err)
	}
}

func TestEnvCopyTdataInZipArchive(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "log.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// A tdata tree inside the zip under a victim folder.
	mustAddZipFile(t, zw, "VictimA/tdata/key_datas", "localkey")
	mustAddZipFile(t, zw, "VictimA/tdata/settingss", "settings")
	mustAddZipFile(t, zw, "VictimA/tdata/D877F783D5D3EF8C/maps0", "maps")
	// A decoy tdata (no key_data*) — must NOT be promoted.
	mustAddZipFile(t, zw, "Decoy/tdata/maps0", "notreal")
	// A decoy that shares the prefix "tdata" but is not a folder signal.
	mustAddZipFile(t, zw, "other.txt", "junk")
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()

	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (only the confirmed tdata)", es.DirsCopied)
	}
	dst := filepath.Join(root, "tdata")
	if _, err := os.Stat(filepath.Join(dst, "key_datas")); err != nil {
		t.Fatalf("confirmed tdata key_datas missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "D877F783D5D3EF8C", "maps0")); err != nil {
		t.Fatalf("nested maps0 missing: %v", err)
	}
	// Decoy tdata must not have been promoted.
	if _, err := os.Stat(filepath.Join(root, "tdata_2")); !os.IsNotExist(err) {
		t.Fatalf("decoy tdata should not be promoted: %v", err)
	}
}

func mustAddZipFile(t *testing.T, zw *zip.Writer, name, body string) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(body))
}

func TestEnvCopyTdataInRarArchive(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	// Build a tdata tree on disk, then pack it into a rar (preserving paths).
	td := filepath.Join(dir, "src", "VictimA", "tdata")
	if err := os.MkdirAll(filepath.Join(td, "D877F783D5D3EF8C"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("localkey"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "D877F783D5D3EF8C", "maps0"), []byte("maps"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rarBin, "a", "-m0", "-ep1", "-idq", "log.rar", "src/VictimA/tdata")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar pack failed (%v): %s", e, out)
	}
	rarPath := filepath.Join(dir, "log.rar")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), rarPath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (copied=%d errors=%d)", es.DirsCopied, es.Copied, es.WriteErrors)
	}
	dst := filepath.Join(root, "tdata")
	if _, err := os.Stat(filepath.Join(dst, "key_datas")); err != nil {
		t.Fatalf("rar tdata key_datas missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "D877F783D5D3EF8C", "maps0")); err != nil {
		t.Fatalf("rar nested maps0 missing: %v", err)
	}
}

func TestEnvCopyTdataIn7zArchive(t *testing.T) {
	bin := first7z()
	if bin == "" {
		t.Skip("no 7z packer found")
	}
	dir := t.TempDir()
	td := filepath.Join(dir, "src", "VictimA", "tdata")
	if err := os.MkdirAll(filepath.Join(td, "D877F783D5D3EF8C"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("localkey"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "D877F783D5D3EF8C", "maps0"), []byte("maps"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "a", "-y", "-bso0", "-bsp0", "log.7z", "src/VictimA/tdata")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("7z create failed: %v\n%s", e, out)
	}
	path := filepath.Join(dir, "log.7z")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), path, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (copied=%d errors=%d)", es.DirsCopied, es.Copied, es.WriteErrors)
	}
	dst := filepath.Join(root, "tdata")
	if _, err := os.Stat(filepath.Join(dst, "key_datas")); err != nil {
		t.Fatalf("7z tdata key_datas missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "D877F783D5D3EF8C", "maps0")); err != nil {
		t.Fatalf("7z nested maps0 missing: %v", err)
	}
}

func TestEnvCopyKeyData2sLoose(t *testing.T) {
	dir := t.TempDir()
	td := filepath.Join(dir, "VictimA", "tdata")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_data#2s"), []byte("acct2"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1", es.DirsCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_data#2s")); err != nil {
		t.Fatalf("key_data#2s not copied: %v", err)
	}
}

func TestEnvCopyKeyData2sInZip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "log.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mustAddZipFile(t, zw, "VictimA/tdata/key_data#2s", "acct2")
	mustAddZipFile(t, zw, "VictimA/tdata/maps0", "maps")
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1", es.DirsCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_data#2s")); err != nil {
		t.Fatalf("zip key_data#2s missing: %v", err)
	}
}

func TestEnvCopyEncryptedTdataOnlyZip(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "locked.zip")
	writeEncryptedTestZip(t, archivePath, "ice", "Victim/tdata/key_datas", "localkey")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{"", "ice"}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (encrypted tdata-only zip); copied=%d errors=%d",
			es.DirsCopied, es.Copied, es.WriteErrors)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_datas")); err != nil {
		t.Fatalf("encrypted zip tdata missing: %v", err)
	}
}

func TestEnvCopyNestedZipTdata(t *testing.T) {
	inner := zipBytes(t, map[string][]byte{
		"Victim/tdata/key_datas": []byte("localkey"),
		"Victim/tdata/settingss": []byte("s"),
	})
	outer := filepath.Join(t.TempDir(), "outer.zip")
	writeZipMembers(t, outer, map[string][]byte{"inner.zip": inner})

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), outer, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (nested zip tdata)", es.DirsCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_datas")); err != nil {
		t.Fatalf("nested zip tdata missing: %v", err)
	}
}

func TestEnvCopyZipDecoyOnly(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "decoy.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mustAddZipFile(t, zw, "Decoy/tdata/maps0", "notreal")
	mustAddZipFile(t, zw, "other.txt", "junk")
	_ = zw.Close()
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{""}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), archivePath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 0 {
		t.Fatalf("DirsCopied = %d, want 0 (decoy tdata only)", es.DirsCopied)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata")); !os.IsNotExist(err) {
		t.Fatalf("decoy tdata should not be promoted: %v", err)
	}
}

func TestUniqueDirExhaustion(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "tdata")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 1000; i++ {
		if err := os.Mkdir(base+"_"+itoa(i), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := uniqueDir(base); ok {
		t.Fatal("expected uniqueDir exhaustion")
	}
}

func TestReserveTdataDestExhaustion(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "tdata"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tdata", "marker"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 1000; i++ {
		if err := os.Mkdir(filepath.Join(root, "tdata_"+itoa(i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(t.TempDir(), "tdata")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "key_datas"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	if err := copier.CopyDir(src); err == nil {
		t.Fatal("expected reserveTdataDest exhaustion")
	}
	got, err := os.ReadFile(filepath.Join(root, "tdata", "marker"))
	if err != nil || string(got) != "keep" {
		t.Fatalf("existing tdata was clobbered: %s err=%v", got, err)
	}
	es := copier.Close()
	if es.DirsCopied != 0 {
		t.Fatalf("DirsCopied = %d, want 0", es.DirsCopied)
	}
	if es.WriteErrors == 0 {
		t.Fatal("expected a write error on exhaustion")
	}
}

func TestPromoteTdataCrossDevice(t *testing.T) {
	secrets := t.TempDir()
	copier := NewEnvCopier(secrets, nil, defaultEnvCopyMaxLen)
	stageBase := crossDeviceTemp(t, secrets)
	if stageBase == "" {
		stageBase = t.TempDir()
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(stageBase) })
	}
	staged := filepath.Join(stageBase, "tdata")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "key_datas"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copier.PromoteTdata(staged); err != nil {
		t.Fatalf("PromoteTdata: %v", err)
	}
	if _, err := os.Stat(filepath.Join(secrets, "tdata", "key_datas")); err != nil {
		t.Fatalf("promoted key_datas missing: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staging should be consumed, stat err=%v", err)
	}
}

// crossDeviceTemp returns a temp dir on a different device than destRoot when
// /dev/shm (or similar) is mounted separately. Empty means none found.
func crossDeviceTemp(t *testing.T, destRoot string) string {
	t.Helper()
	for _, base := range []string{"/dev/shm"} {
		d, err := os.MkdirTemp(base, "sfl-tdata-exdev-*")
		if err != nil {
			continue
		}
		probeSrc := filepath.Join(d, ".probe")
		probeDst := filepath.Join(destRoot, ".probe")
		if err := os.WriteFile(probeSrc, []byte("x"), 0o600); err != nil {
			_ = os.RemoveAll(d)
			continue
		}
		err = os.Rename(probeSrc, probeDst)
		_ = os.Remove(probeSrc)
		_ = os.Remove(probeDst)
		if err == nil {
			_ = os.RemoveAll(d)
			continue
		}
		t.Logf("cross-device stage %s (%v)", d, err)
		return d
	}
	t.Log("no cross-device filesystem; PromoteTdata still exercised on same FS")
	return ""
}

func TestConcurrentTdataCopyNoMerge(t *testing.T) {
	root := t.TempDir()
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	var srcs [2]string
	for i, body := range []string{"alpha", "beta"} {
		td := filepath.Join(t.TempDir(), "tdata")
		if err := os.Mkdir(td, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		srcs[i] = td
	}
	var wg sync.WaitGroup
	errs := make([]error, len(srcs))
	for i, src := range srcs {
		wg.Add(1)
		go func(i int, src string) {
			defer wg.Done()
			errs[i] = copier.CopyDir(src)
		}(i, src)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("CopyDir[%d]: %v", i, err)
		}
	}
	es := copier.Close()
	if es.DirsCopied != 2 {
		t.Fatalf("DirsCopied = %d, want 2", es.DirsCopied)
	}
	a, err := os.ReadFile(filepath.Join(root, "tdata", "key_datas"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "tdata_2", "key_datas"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(a) + string(b)
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Fatalf("trees merged or lost a victim: %q / %q", a, b)
	}
}

func TestCopyTreeSkipsFifo(t *testing.T) {
	td := filepath.Join(t.TempDir(), "tdata")
	if err := os.Mkdir(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(td, "hang")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Skipf("mkfifo: %v %s", err, out)
	}
	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	done := make(chan error, 1)
	go func() { done <- copier.CopyDir(td) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CopyDir: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("copyTree hung on fifo")
	}
	if _, err := os.Lstat(filepath.Join(root, "tdata", "hang")); !os.IsNotExist(err) {
		t.Fatalf("fifo should not be copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_datas")); err != nil {
		t.Fatalf("key_datas missing: %v", err)
	}
}

func TestEnvCopyEncryptedTdataRar(t *testing.T) {
	rarBin, err := exec.LookPath("rar")
	if err != nil {
		t.Skip("no rar packer found")
	}
	dir := t.TempDir()
	td := filepath.Join(dir, "src", "VictimA", "tdata")
	if err := os.MkdirAll(td, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "key_datas"), []byte("localkey"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rarBin, "a", "-m0", "-hpice", "-ep1", "-idq", "enc.rar", "src/VictimA/tdata")
	cmd.Dir = dir
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Skipf("rar encrypt pack failed (%v): %s", e, out)
	}
	rarPath := filepath.Join(dir, "enc.rar")

	root := filepath.Join(t.TempDir(), "secrets")
	copier := NewEnvCopier(root, nil, defaultEnvCopyMaxLen)
	copier.Start()
	e := &Engine{Workers: 1, EnvCopier: copier, Passwords: []string{"", "ice"}}
	var out strings.Builder
	if _, _, err := e.Run(context.Background(), rarPath, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	es := copier.Close()
	if es.DirsCopied != 1 {
		t.Fatalf("DirsCopied = %d, want 1 (encrypted rar tdata); copied=%d errors=%d",
			es.DirsCopied, es.Copied, es.WriteErrors)
	}
	if _, err := os.Stat(filepath.Join(root, "tdata", "key_datas")); err != nil {
		t.Fatalf("encrypted rar tdata missing: %v", err)
	}
}
