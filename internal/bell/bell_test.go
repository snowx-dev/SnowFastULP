package bell

import (
	"os/exec"
	"runtime"
	"sync/atomic"
	"testing"
)

func successCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "exit", "0")
	}
	return exec.Command("true")
}

func TestRingInvokesPlayer(t *testing.T) {
	var calls atomic.Int32
	old := playCmd
	playCmd = func(path string) *exec.Cmd {
		calls.Add(1)
		return successCmd()
	}
	t.Cleanup(func() { playCmd = old })

	Ring()
	if calls.Load() != 1 {
		t.Fatalf("playCmd calls = %d, want 1", calls.Load())
	}
}

func TestRingNoPlayerIsSilent(t *testing.T) {
	old := playCmd
	playCmd = func(string) *exec.Cmd { return nil }
	t.Cleanup(func() { playCmd = old })
	Ring() // must not panic
}
