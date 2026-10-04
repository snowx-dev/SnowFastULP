package bell

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

//go:embed bell.wav
var sound []byte

// playCmd builds the OS player command for path. Nil = nowhere to play (silent).
// Overridable in tests.
var playCmd = defaultPlayCmd

// Ring plays the embedded completion sound. Fail-soft: any error is ignored.
// Blocks until playback finishes or ~5s elapses (so os.Exit does not cut it off).
func Ring() {
	if len(sound) == 0 {
		return
	}
	f, err := os.CreateTemp("", "snowfast-bell-*.wav")
	if err != nil {
		return
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(sound); err != nil {
		f.Close()
		return
	}
	if err := f.Close(); err != nil {
		return
	}
	cmd := playCmd(path)
	if cmd == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
}

func defaultPlayCmd(path string) *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("afplay", path)
	case "windows":
		ps := fmt.Sprintf("(New-Object Media.SoundPlayer '%s').PlaySync()", escapePS(path))
		return exec.Command("powershell", "-NoProfile", "-Command", ps)
	default:
		for _, cand := range [][]string{
			{"paplay", path},
			{"aplay", "-q", path},
			{"ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", path},
		} {
			if p, err := exec.LookPath(cand[0]); err == nil {
				return exec.Command(p, cand[1:]...)
			}
		}
		return nil
	}
}

func escapePS(s string) string {
	// ponytail: enough for temp paths; full PS escaping if paths ever get quotes.
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}
