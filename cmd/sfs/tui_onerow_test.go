package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/termctl"
)

// P5-W9: a one-row terminal must never get an unclamped frame. termHeight()<=1
// made draw pass 0 to tuiframe.Compose, whose nonpositive maxRows means
// UNLIMITED — every frame dumped its full body and scrolled the single row.
// Heights 0, 1, and 2 must render at most one row per frame (bounded newline
// and clear-line escape count), and close must still restore the terminal.
func TestSfsFrameOneRowTerminalBounded(t *testing.T) {
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("row %d", i)
	}
	for _, h := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("height%d", h), func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatalf("temp: %v", err)
			}
			path := f.Name()
			defer os.Remove(path)

			oldStderr := os.Stderr
			os.Stderr = f
			oldHeight := termHeight
			termHeight = func() int { return h }
			t.Cleanup(func() {
				termHeight = oldHeight
				os.Stderr = oldStderr
			})

			frame := stderrFrame{tty: true}
			frame.draw(lines)
			frame.close()
			f.Close()

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			out := string(raw)

			// one rendered row max: Compose emits \n only BETWEEN rows, so a
			// clamped one-row frame contains zero newlines.
			if n := strings.Count(out, "\n"); n > 1 {
				t.Errorf("height %d: %d newlines in frame, want <= 1 (unclamped spam):\n%q", h, n, out)
			}
			// clearLine is emitted once per rendered row
			if n := strings.Count(out, "\033[2K"); n > 1 {
				t.Errorf("height %d: %d rendered rows, want <= 1", h, n)
			}
			// close must still restore the terminal
			if leave := strings.LastIndex(out, termctl.AltScreenLeave); leave < 0 {
				t.Errorf("height %d: close never left the alt screen:\n%q", h, out)
			}
		})
	}
}

// sanity: a normal-height terminal still renders its full frame (the clamp
// must not over-correct and crush the regular path).
func TestSfsFrameNormalHeightStillRenders(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer os.Remove(f.Name())

	oldStderr := os.Stderr
	os.Stderr = f
	oldHeight := termHeight
	termHeight = func() int { return 24 }
	t.Cleanup(func() {
		termHeight = oldHeight
		os.Stderr = oldStderr
	})

	frame := stderrFrame{tty: true}
	frame.draw([]string{"alpha", "beta", "gamma"})
	frame.close()
	f.Close()

	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(out, want) {
			t.Errorf("normal-height frame lost row %q", want)
		}
	}
	if leave := strings.LastIndex(out, termctl.AltScreenLeave); leave < 0 {
		t.Errorf("close never left the alt screen")
	}
}
