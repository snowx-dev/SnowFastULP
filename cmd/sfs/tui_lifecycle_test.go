package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
)

// close must win over a racing ticker draw: once closed, draw never re-enters
// the alt screen, so no AltScreenEnter may follow the final AltScreenLeave.
func TestSfsFrameConcurrentCloseDrawNeverReopens(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	path := f.Name()
	defer os.Remove(path)

	old := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = old
		f.Close()
	})

	frame := stderrFrame{tty: true}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // ticker-equivalent: keep drawing well past the close
		defer wg.Done()
		lines := []string{"indexing", "searching"}
		for i := 0; i < 300; i++ {
			frame.draw(lines)
			time.Sleep(300 * time.Microsecond)
		}
	}()
	go func() { // the cleanup hook / force-exit path
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		frame.close()
	}()
	wg.Wait()
	f.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	leave := strings.LastIndex(out, termctl.AltScreenLeave)
	if leave < 0 {
		t.Fatalf("frame never left the alt screen:\n%q", out)
	}
	if rest := out[leave+len(termctl.AltScreenLeave):]; strings.Contains(rest, termctl.AltScreenEnter) {
		t.Fatalf("frame re-entered the alt screen after close:\n%q", rest)
	}
	// a draw after close must be a no-op
	before := len(raw)
	wf, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	os.Stderr = wf
	frame.draw([]string{"late"})
	os.Stderr = old
	wf.Close()
	after, _ := os.ReadFile(path)
	if len(after) != before {
		t.Fatalf("post-close draw wrote bytes: %q", string(after[before:]))
	}
}

// Frost styling must keep the ❤️ cluster intact; per-rune SGR used to split
// ❤ from VS16, under-count width by 1, and soft-wrap junk onto the next row.
func TestFrostTaglineHeartKeepsEmojiWidth(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	plain := tuiFooterLine1
	styled := renderFrostTagline(plain, 0.0, 0.5)
	if got, want := tuiframe.VisibleWidth(styled), tuiframe.VisibleWidth(plain); got != want {
		t.Fatalf("styled width %d != plain width %d (heart cluster split?)", got, want)
	}
	if !strings.Contains(styled, "❤️") && !strings.Contains(styled, "❤\ufe0f") {
		t.Fatalf("styled footer lost heart cluster: %q", styled)
	}
}

// widths 0-4: no panic, nonpositive budget renders nothing, positive budgets
// are honored by cells for both CJK/emoji and plain taglines.
func TestRenderFrostTaglineRightWidths0to4(t *testing.T) {
	for _, text := range []string{"sfs is open-source ❤️", "冰蓝渐变 · 中文标语", "👨‍👩‍👧 emoji line"} {
		for _, width := range []int{0, -3, 1, 2, 3, 4, 24} {
			got := renderFrostTaglineRight(text, width, 0.0, 0.5)
			if width <= 0 {
				if got != "" {
					t.Errorf("width=%d: got %q, want empty", width, got)
				}
				continue
			}
			if w := tuiframe.VisibleWidth(got); w > width {
				t.Errorf("text=%q width=%d: row width %d exceeds budget: %q", text, width, w, got)
			}
			if strings.Contains(got, "\ufffd") {
				t.Errorf("text=%q width=%d: mojibake in %q", text, width, got)
			}
		}
	}
}

// footer rows at widths 0-4 must never panic and, for positive widths, never
// exceed the requested row budget.
func TestSfsFooterRowsWidths0to4(t *testing.T) {
	notice := selfupdateNotice(t)
	for _, width := range []int{0, 1, 2, 3, 4, 24} {
		for _, lines := range [][]string{
			renderSummaryFooter(width, notice),
			renderLiveScreenFooter(width),
		} {
			for _, ln := range lines {
				_ = tuiframe.VisibleWidth(ln) // any panic here fails the test
				if width >= 1 && tuiframe.VisibleWidth(ln) > width {
					t.Errorf("width=%d: row width %d exceeds budget: %q", width, tuiframe.VisibleWidth(ln), ln)
				}
			}
		}
	}
}

func selfupdateNotice(t *testing.T) *selfupdate.Notice {
	t.Helper()
	return &selfupdate.Notice{Latest: "v9.9", Command: "go install ..."}
}
