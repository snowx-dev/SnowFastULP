// Package tuiframe centralizes the terminal control sequence that the sfu/sfs/
// sfl live status blocks share, so the whole CLI family gets one correct,
// ghost-free in-place redraw instead of three slightly different ones.
//
// The sequence is deliberately conservative: home the cursor, rewrite every
// line, emit a newline only BETWEEN lines (never after the last so the final
// line can't push the screen into a scroll), then erase everything below to
// wipe a taller previous frame's stale rows.
package tuiframe

import "strings"

const (
	cursorHome = "\033[H"
	clearLine  = "\033[2K\r"
	eraseBelow = "\033[J"
)

// Compose builds the in-place redraw byte sequence for lines. lines are clamped
// to maxRows (<= 0 means no clamp). It returns "" when there is nothing to draw
// so callers can cheaply skip a write.
//
// The trailing newline after the last line is intentionally omitted: writing it
// on the bottom row scrolls the buffer up by one, which is the root of the
// "frame creeps upward / footer ghosts" class of bug. eraseBelow then clears
// any rows a previous, taller frame left behind.
func Compose(lines []string, maxRows int) string {
	if len(lines) == 0 {
		return ""
	}
	if maxRows > 0 && len(lines) > maxRows {
		lines = lines[:maxRows]
	}
	var b strings.Builder
	b.WriteString(cursorHome)
	for i, ln := range lines {
		b.WriteString(clearLine)
		b.WriteString(ln)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteString(eraseBelow)
	return b.String()
}
