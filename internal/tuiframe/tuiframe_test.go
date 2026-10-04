package tuiframe

import (
	"strings"
	"testing"
)

func TestComposeEmptyReturnsEmpty(t *testing.T) {
	if got := Compose(nil, 0); got != "" {
		t.Fatalf("Compose(nil) = %q, want empty", got)
	}
	if got := Compose([]string{}, 10); got != "" {
		t.Fatalf("Compose([]) = %q, want empty", got)
	}
}

func TestComposeHomesAndErasesBelow(t *testing.T) {
	out := Compose([]string{"a", "b"}, 0)
	if !strings.HasPrefix(out, cursorHome) {
		t.Fatalf("frame must start by homing the cursor: %q", out)
	}
	if !strings.HasSuffix(out, eraseBelow) {
		t.Fatalf("frame must end by erasing below: %q", out)
	}
}

func TestComposeNoTrailingNewlineAfterLastLine(t *testing.T) {
	// A newline only BETWEEN lines: N lines => N-1 newlines. The bottom line
	// must not be followed by a newline (that scrolls the buffer).
	out := Compose([]string{"x", "y", "z"}, 0)
	if n := strings.Count(out, "\n"); n != 2 {
		t.Fatalf("3 lines should yield 2 separators, got %d in %q", n, out)
	}
	// The erase-below code must come immediately after the last line's content,
	// not after a newline.
	if !strings.HasSuffix(out, "z"+eraseBelow) {
		t.Fatalf("last line must be followed directly by erase-below: %q", out)
	}
}

func TestComposeClampsToMaxRows(t *testing.T) {
	lines := []string{"1", "2", "3", "4", "5"}
	out := Compose(lines, 3)
	if strings.Contains(out, "4") || strings.Contains(out, "5") {
		t.Fatalf("rows beyond maxRows must be dropped: %q", out)
	}
	if !strings.Contains(out, "3") {
		t.Fatalf("rows within maxRows must be kept: %q", out)
	}
	// clamped to 3 rows => 2 separators
	if n := strings.Count(out, "\n"); n != 2 {
		t.Fatalf("clamped frame should have 2 separators, got %d", n)
	}
}

func TestComposeMaxRowsZeroOrNegativeNoClamp(t *testing.T) {
	lines := []string{"1", "2", "3"}
	for _, max := range []int{0, -1} {
		out := Compose(lines, max)
		for _, ln := range lines {
			if !strings.Contains(out, ln) {
				t.Fatalf("maxRows=%d dropped line %q: %q", max, ln, out)
			}
		}
	}
}

func TestComposeEachLineCleared(t *testing.T) {
	out := Compose([]string{"a", "b"}, 0)
	if n := strings.Count(out, clearLine); n != 2 {
		t.Fatalf("each line must be cleared before writing, got %d clears: %q", n, out)
	}
}
