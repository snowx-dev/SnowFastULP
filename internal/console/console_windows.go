//go:build windows

package console

import (
	"os"

	"golang.org/x/sys/windows"
)

// flips ENABLE_VIRTUAL_TERMINAL_PROCESSING on stderr/stdout.
// older consoles ignore the bit, non-TTY returns ERROR_INVALID_HANDLE which we swallow
func platformEnableVT() {
	for _, f := range []*os.File{os.Stderr, os.Stdout} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(h, &mode); err != nil {
			continue
		}
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}
