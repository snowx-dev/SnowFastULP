package tuiframe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncatePathShortUnchanged(t *testing.T) {
	p := "/data/Library"
	if got := TruncatePath(p, 40); got != p {
		t.Fatalf("TruncatePath(%q, 40) = %q, want unchanged", p, got)
	}
}

func TestTruncatePathKeepsSuffix(t *testing.T) {
	p := "/run/media/bigboi/b992e755-aaaa-bbbb-cccc-dddddddddddd/Data_logs/deep/nested/file.txt"
	got := TruncatePath(p, 24)
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("expected ellipsis prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "file.txt") {
		t.Fatalf("expected path tail kept, got %q", got)
	}
	if strings.Contains(got, "b992e755") {
		t.Fatalf("head UUID should be cut, got %q", got)
	}
	r := []rune(got)
	if len(r) != 24 {
		t.Fatalf("visible rune length = %d, want 24 (%q)", len(r), got)
	}
}

func TestTruncatePathClampsMaxBelow8(t *testing.T) {
	p := "/aaaaaaaa/bbbbbbbb/cccccccc"
	got := TruncatePath(p, 3)
	r := []rune(got)
	if len(r) != 8 {
		t.Fatalf("max<8 should clamp to 8 runes, got %d (%q)", len(r), got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}

func TestTruncatePathUTF8Safe(t *testing.T) {
	// Nested separator used by sfl worker labels; must not slice mid-rune.
	p := "outer.rar ▸ " + strings.Repeat("ä", 40) + "/inner.7z"
	got := TruncatePath(p, 20)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated path not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "inner.7z") {
		t.Fatalf("expected tail kept, got %q", got)
	}
	if strings.Contains(got, "\ufffd") {
		t.Fatalf("mojibake replacement in %q", got)
	}
}
