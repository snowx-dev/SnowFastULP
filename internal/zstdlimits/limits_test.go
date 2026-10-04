package zstdlimits_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/snowx-dev/SnowFastULP/internal/zstdlimits"
)

// TestExplainWrapsWindowErrors: Explain must recognize the klauspost
// window/decoder-size sentinels and wrap them with the 128 MiB cap and the
// re-compression remediation (no zstd --long, or window ≤128 MiB), while
// passing every other error through unchanged.
func TestExplainWrapsWindowErrors(t *testing.T) {
	for _, sent := range []error{zstd.ErrWindowSizeExceeded, zstd.ErrDecoderSizeExceeded} {
		wrapped := zstdlimits.Explain(sent)
		if wrapped == sent {
			t.Fatalf("%v must be wrapped, not returned as-is", sent)
		}
		if !errors.Is(wrapped, sent) {
			t.Fatalf("wrapped error lost the sentinel: %v", wrapped)
		}
		msg := wrapped.Error()
		for _, want := range []string{"128 MiB", "--long"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("message for %v missing %q:\n%s", sent, want, msg)
			}
		}
	}

	plain := errors.New("plain failure")
	if got := zstdlimits.Explain(plain); got != plain {
		t.Fatalf("non-window error must pass through unchanged, got %v", got)
	}
	if got := zstdlimits.Explain(nil); got != nil {
		t.Fatalf("Explain(nil) = %v, want nil", got)
	}
}
