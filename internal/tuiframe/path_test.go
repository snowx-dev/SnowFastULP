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
	p := "/run/media/user/012e7890-aaaa-bbbb-cccc-dddddddddddd/Data_logs/deep/nested/file.txt"
	got := TruncatePath(p, 24)
	if !strings.HasPrefix(got, "…") {
		t.Fatalf("expected ellipsis prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "file.txt") {
		t.Fatalf("expected path tail kept, got %q", got)
	}
	if strings.Contains(got, "012e7890") {
		t.Fatalf("head UUID should be cut, got %q", got)
	}
	r := []rune(got)
	if len(r) != 24 {
		t.Fatalf("visible rune length = %d, want 24 (%q)", len(r), got)
	}
}

func TestTruncatePathHonorsBudgetsBelow8(t *testing.T) {
	p := "/aaaaaaaa/bbbbbbbb/cccccccc"
	for _, max := range []int{1, 2, 3, 7} {
		got := TruncatePath(p, max)
		if w := VisibleWidth(got); w != max {
			t.Fatalf("max=%d should be honored: VisibleWidth = %d (%q)", max, w, got)
		}
		if !strings.HasPrefix(got, "…") {
			t.Fatalf("expected ellipsis, got %q", got)
		}
	}
}

func TestTruncatePathZeroBudgetYieldsEmpty(t *testing.T) {
	for _, max := range []int{0, -4} {
		if got := TruncatePath("/x/y", max); got != "" {
			t.Fatalf("TruncatePath with max=%d = %q, want empty", max, got)
		}
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
