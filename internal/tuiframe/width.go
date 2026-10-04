package tuiframe

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// The width helpers below are the single source of truth for "how many
// terminal cells does this string occupy" across sfu/sfs/sfl. They measure in
// terminal CELLS (wide CJK counts 2, combining marks 0), never split grapheme
// clusters (ZWJ emoji families, combining sequences), and always preserve ANSI
// escape sequences so styled lines survive truncation intact.

// VisibleWidth measures s in terminal cells, ignoring ANSI escape sequences
// and accounting for wide runes and grapheme clusters.
func VisibleWidth(s string) int {
	return ansi.StringWidth(s)
}

// TruncateRight keeps the leftmost max cells of s, dropping the tail. Every
// positive budget is honored (including below eight); max <= 0 yields "".
// ANSI sequences are preserved and no grapheme cluster is split.
func TruncateRight(s string, max int) string {
	if max <= 0 {
		return ""
	}
	return ansi.Truncate(s, max, "")
}

// TruncateLeft keeps the rightmost max cells of s, marking the cut with an
// ellipsis. Every positive budget is honored; max <= 0 yields "". ANSI
// sequences are preserved and no grapheme cluster is split.
func TruncateLeft(s string, max int) string {
	if max <= 0 {
		return ""
	}
	w := VisibleWidth(s)
	if w <= max {
		return s
	}
	// Keep the rightmost max-1 cells (the ellipsis owns one cell).
	// ansi.TruncateLeft removes the head by grapheme clusters and can
	// overshoot by up to one wide cluster since it has no right budget, so
	// clamp the tail to exactly max-1 cells with TruncateRight. Both calls
	// preserve ANSI sequences and never split grapheme clusters.
	tail := ansi.TruncateLeft(s, w-(max-1), "")
	return "…" + ansi.Truncate(tail, max-1, "")
}

// PadOrTrim pads s with trailing spaces — or trims it — to exactly max
// terminal cells. Every positive budget is honored; max <= 0 yields "".
func PadOrTrim(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if w := VisibleWidth(s); w < max {
		return s + strings.Repeat(" ", max-w)
	}
	return TruncateRight(s, max)
}
