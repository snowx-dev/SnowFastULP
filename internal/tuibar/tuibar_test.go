package tuibar_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/snowx-dev/SnowFastULP/internal/tuibar"
)

func testStyle() tuibar.Style {
	return tuibar.Style{
		Label: lipgloss.NewStyle().Bold(true),
		Muted: lipgloss.NewStyle(),
		Count: lipgloss.NewStyle(),
		Byte:  lipgloss.NewStyle(),
		Bar: func(percent float64, width int) string {
			body := width - 7 // " 100.0%"
			if body < 1 {
				body = 1
			}
			fill := int(float64(body) * percent)
			return strings.Repeat("█", fill) + strings.Repeat("░", body-fill) +
				fmt.Sprintf(" %5.1f%%", percent*100)
		},
		Bytes: func(n int64) string { return fmt.Sprintf("%dB", n) },
		Rate:  func(bps float64) string { return fmt.Sprintf("%.0fBps", bps) },
	}
}

func TestBarDrawsAllSegments(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "history · checking", func() int { return 100 })
	bar.Update(3, 10, 300, 1000)
	bar.Update(10, 10, 1000, 1000) // final frame must always draw
	out := buf.String()
	for _, want := range []string{
		"history · checking",
		"█",
		"░",
		" 30.0%",
		"3/10 files",
		"300B / 1000B",
		"Bps",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("bar output missing %q:\n%q", want, out)
		}
	}
	if !strings.Contains(out, "10/10 files") {
		t.Fatalf("final frame missing completed counts:\n%q", out)
	}
}

func TestBarFinishErasesWidestFrame(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "history · validating", func() int { return 100 })
	bar.Update(1, 4, 10, 1000)
	bar.Update(4, 4, 1000, 1000)
	bar.Finish()
	out := buf.String()
	if !strings.HasSuffix(out, "\r") {
		t.Fatalf("finish must leave the cursor at column 0: %q", out)
	}
	parts := strings.Split(out, "\r")
	clearing := parts[len(parts)-2]
	widest := 0
	for _, seg := range parts[:len(parts)-1] {
		if w := tuibar.VisibleWidth(seg); w > widest {
			widest = w
		}
	}
	if widest == 0 {
		t.Fatalf("no frames drawn: %q", out)
	}
	if tuibar.VisibleWidth(clearing) < widest {
		t.Fatalf("finish cleared %q (%d cells) but widest frame was %d cells; stale tail would remain:\n%q",
			clearing, tuibar.VisibleWidth(clearing), widest, out)
	}
}

func TestBarThrottlesRapidUpdates(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "history · checking", func() int { return 100 })
	for i := 0; i <= 50; i++ {
		bar.Update(i, 50, int64(i), 50)
		time.Sleep(time.Millisecond)
	}
	// first frame draws immediately, interior updates throttle to the
	// ~100ms window, final (complete) frame always draws: at most a
	// handful of redraws for 51 updates.
	frames := strings.Count(buf.String(), "\r")
	if frames < 2 || frames > 5 {
		t.Fatalf("got %d frames for 51 sub-throttle updates: %q", frames, buf.String())
	}
}

func TestBarDegradesOnNarrowWidth(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "history · checking", func() int { return 30 })
	bar.Update(10, 10, 1000, 1000)
	out := buf.String()
	if !strings.Contains(out, "history · checking") {
		t.Fatalf("narrow bar dropped the title: %q", out)
	}
	for _, frame := range strings.Split(out, "\r") {
		if w := tuibar.VisibleWidth(frame); w > 30 {
			t.Fatalf("frame %q is %d cells wide, exceeds 30-col terminal", frame, w)
		}
	}
}

func TestBarVeryNarrowKeepsTitleOnly(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "history · checking", func() int { return 12 })
	bar.Update(1, 1, 5, 10)
	out := buf.String()
	for _, frame := range strings.Split(out, "\r") {
		if w := tuibar.VisibleWidth(frame); w > 12 {
			t.Fatalf("frame %q is %d cells wide, exceeds 12-col terminal", frame, w)
		}
	}
	if !strings.Contains(out, "history") {
		t.Fatalf("even the narrowest bar should keep a truncated title: %q", out)
	}
}

func TestBarZeroByteTotalCompletes(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "history · checking", func() int { return 80 })
	bar.Update(1, 1, 0, 0) // empty sources: bytes never move
	out := buf.String()
	if !strings.Contains(out, " 100.0%") {
		t.Fatalf("zero-byte total never reached 100%%: %q", out)
	}
	if !strings.Contains(out, "1/1 file") {
		t.Fatalf("zero-byte total missing file count: %q", out)
	}
}

func TestBarSeqProgressAggregatesSequentialPass(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "history · validating", func() int { return 100 })
	seq := bar.SeqProgress(2, 30)
	seq("/first", 5, 10)
	seq("/first", 10, 10) // first file complete: 10 bytes
	seq("/second", 10, 20)
	seq("/second", 20, 20)   // second file complete: 20 bytes → 30 total
	bar.Update(2, 2, 30, 30) // caller flushes the completed frame
	bar.Finish()
	out := buf.String()
	if !strings.Contains(out, "2/2 files") {
		t.Fatalf("sequential pass never counted both files:\n%q", out)
	}
	if !strings.Contains(out, "30B / 30B") {
		t.Fatalf("sequential aggregate bytes wrong:\n%q", out)
	}
}

func TestBarNilSafety(t *testing.T) {
	var buf bytes.Buffer
	bar := tuibar.New(&buf, testStyle(), "t", nil) // nil width fn → default
	bar.Update(1, 1, 1, 1)
	bar.Finish()

	var nilBar *tuibar.Bar
	nilBar.Update(1, 1, 1, 1)
	nilBar.Finish()
	nilBar.SeqProgress(1, 1)("/x", 1, 1) // nil receiver must not panic

	noWriter := tuibar.New(nil, testStyle(), "t", func() int { return 80 })
	noWriter.Update(1, 1, 1, 1)
	noWriter.Finish()
}
