//go:build windows

package console

import (
	"os"

	"golang.org/x/sys/windows"
)

// platformEnableVT flips ENABLE_VIRTUAL_TERMINAL_PROCESSING on stderr/stdout and
// reports whether stderr ends up VT-capable. A redirected handle (non-console)
// returns ERROR_INVALID_HANDLE from GetConsoleMode and is skipped — VT is moot
// there since the TTY gate disables the TUI, so it does not count as failure. A
// legacy console silently ignores the bit (with or without an error), so we
// re-read and confirm the bit actually stuck; if it didn't on stderr, we report
// false so the caller drops to plain output instead of leaking raw escapes.
func platformEnableVT() bool {
	stderrOK := true
	for _, f := range []*os.File{os.Stderr, os.Stdout} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(h, &mode); err != nil {
			continue // not a console; TTY gate handles it
		}
		if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
			_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
		}
		var after uint32
		if err := windows.GetConsoleMode(h, &after); err != nil ||
			after&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
			if f == os.Stderr {
				stderrOK = false
			}
		}
	}
	return stderrOK
}
