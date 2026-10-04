package zstdlimits

import (
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// MaxDecoderWindow bounds memory requested by a zstd frame from untrusted
// inputs. It covers sfu's output and standard zstd long-window archives while
// preventing one crafted frame from multiplying allocations across workers.
const MaxDecoderWindow = 128 << 20

// Explain recognizes the klauspost window/decoder-size sentinels and wraps
// them with the decoder cap and the remediation, so a rejected long-window
// frame tells the user exactly why and how to fix the input. Any other error
// (including nil) passes through unchanged — same value, no extra wrapping.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, zstd.ErrWindowSizeExceeded) || errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		return fmt.Errorf(
			"zstd frame needs a window above the %d MiB decoder cap: re-compress without zstd --long, or with a window size ≤%d MiB (%w)",
			MaxDecoderWindow>>20, MaxDecoderWindow>>20, err)
	}
	return err
}
