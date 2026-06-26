package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunODInvokesSFUWithGeneratedULP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake sfu")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "logs", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("URL: https://od.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	libDir := filepath.Join(dir, "library")
	argsPath := filepath.Join(dir, "args.txt")
	ulpPath := filepath.Join(dir, "ulp.txt")
	fakeSFU := filepath.Join(dir, "sfu")
	script := "#!/bin/sh\ncp \"$1\" \"$SFL_TEST_ULP\"\nprintf '%s\\n' \"$@\" > \"$SFL_TEST_ARGS\"\n"
	if err := os.WriteFile(fakeSFU, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SFL_SFU_BIN", fakeSFU)
	t.Setenv("SFL_TEST_ARGS", argsPath)
	t.Setenv("SFL_TEST_ULP", ulpPath)

	if err := run(runConfig{
		Input: input, LibraryDir: libDir, Workers: 1, NoTUI: true,
		Started: time.Date(2026, 6, 26, 21, 2, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	ulp, err := os.ReadFile(ulpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ulp) != "od.example.com/login:u:p\n" {
		t.Fatalf("generated ulp = %q", string(ulp))
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	joined := "\n" + string(args)
	for _, want := range []string{"-od\n" + libDir, "-no-tui"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("sfu args missing %q:\n%s", want, string(args))
		}
	}
}

func TestRunODWithRealSFUCreatesLibraryArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("builds sfu binary")
	}
	dir := t.TempDir()
	sfuBin := filepath.Join(dir, "sfu")
	if runtime.GOOS == "windows" {
		sfuBin += ".exe"
	}
	build := exec.Command("go", "build", "-o", sfuBin, "./cmd/sfu")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build sfu: %v\n%s", err, out)
	}
	t.Setenv("SFL_SFU_BIN", sfuBin)

	input := filepath.Join(dir, "logs", "victim")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "Passwords.txt"), []byte("URL: https://real.example.com/login\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	libDir := filepath.Join(dir, "library")
	if err := run(runConfig{
		Input: input, LibraryDir: libDir, Workers: 1, NoTUI: true, NoUpdateCheck: true,
		Started: time.Date(2026, 6, 26, 21, 3, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	archives, err := filepath.Glob(filepath.Join(libDir, "sfu_*.txt.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives = %v", archives)
	}
	sidecars, err := filepath.Glob(filepath.Join(libDir, "sfu_dedup_idx", "*.idx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sidecars) != 1 {
		t.Fatalf("sidecars = %v", sidecars)
	}
}
