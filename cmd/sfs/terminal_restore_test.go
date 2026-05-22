package main

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

func TestRestoreTerminalNilSafe(t *testing.T) {
	restoreTerminal()
	clearTerminalRestore()
	restoreTerminal()
}

func TestExitWithCodeRestores(t *testing.T) {
	called := false
	setTerminalRestore(func() { called = true })
	defer clearTerminalRestore()
	// cant call exitWithCode in test, hit restore path directly
	restoreTerminal()
	if !called {
		t.Fatal("expected registered terminal restore to run")
	}
}

func TestRestoreTerminalWithoutRegisteredRestoreDoesNotEmitAltScreenLeave(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty-backed terminal restore test is Unix-only")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable for pty-backed terminal restore test")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	script := `
import os, pty, select, sys

exe = os.environ["SFS_TEST_EXE"]
pid, fd = pty.fork()
if pid == 0:
    os.execve(exe, [exe, "-test.run=^TestHelperRestoreTerminalNoRegisteredRestore$"], os.environ.copy())

out = bytearray()
while True:
    ready, _, _ = select.select([fd], [], [], 5)
    if not ready:
        break
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        break
    if not chunk:
        break
    out.extend(chunk)

_, status = os.waitpid(pid, 0)
sys.stdout.buffer.write(out)
code = os.waitstatus_to_exitcode(status)
if code != 0:
    sys.exit(code)
`
	cmd := exec.Command(python, "-c", script)
	cmd.Env = append(os.Environ(),
		"SFS_TEST_EXE="+exe,
		"SFS_RESTORE_HELPER=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pty helper failed: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte(altScreenLeave)) {
		t.Fatalf("restoreTerminal emitted alt-screen leave without registered restore: %q", out)
	}
	if bytes.Contains(out, []byte(ansiResetScroll)) {
		t.Fatalf("restoreTerminal emitted scroll-region reset without registered restore: %q", out)
	}
}

func TestHelperRestoreTerminalNoRegisteredRestore(t *testing.T) {
	if os.Getenv("SFS_RESTORE_HELPER") != "1" {
		return
	}
	clearTerminalRestore()
	restoreTerminal()
	os.Exit(0)
}
