package main

import (
	"os"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"

	"github.com/charmbracelet/x/term"
)

const (
	tuiDisplayWidth = 86
	ansiReset       = "\033[0m"
)

func termWidth() int {
	w := termWidthFull()
	if w > tuiDisplayWidth {
		return tuiDisplayWidth
	}
	return w
}

func termWidthFull() int {
	w, _, err := term.GetSize(os.Stderr.Fd())
	if err != nil || w <= 0 {
		return tuiDisplayWidth
	}
	return w
}

// termHeight reports stderr's row count; a var so tests can inject terminal
// heights (the stderrIsTTY var-seam pattern).
var termHeight = func() int {
	_, h, err := term.GetSize(os.Stderr.Fd())
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// tuiVisibleWidth measures printable terminal cells, skipping ANSI escapes so
// styled lines measure by what the terminal actually shows. Cell measurement
// (wide CJK = 2, grapheme clusters unsplit) lives in the shared tuiframe
// helpers.
func tuiVisibleWidth(s string) int {
	return tuiframe.VisibleWidth(s)
}

// trimToDisplayWidth clamps a (possibly ANSI-styled) line to max printable
// columns, appending an ellipsis so the cut is visible. Frame rows are run
// through this before tuiframe.Compose so a line never exceeds the terminal
// width and soft-wraps (which would desync Compose's per-row cursor math).
// The shared helper owns ANSI preservation and grapheme-safety.
func trimToDisplayWidth(s string, max int) string {
	if max < 1 {
		max = 1
	}
	if tuiVisibleWidth(s) <= max {
		return s
	}
	return tuiframe.TruncateRight(s, max-1) + ansiReset + "…"
}

func padOrTrim(s string, w int) string {
	if w <= 0 {
		return ""
	}
	vw := tuiVisibleWidth(s)
	if vw == w {
		return s
	}
	if vw < w {
		return s + strings.Repeat(" ", w-vw)
	}
	return trimToDisplayWidth(s, w)
}
