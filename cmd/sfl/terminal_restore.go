package main

import (
	"os"
	"sync"
)

const ansiResetScroll = "\033[r"

// terminalRestore holds the live TUI's restore hook so any exit path (force
// quit on a second Ctrl-C, fatal error) can leave the alt-screen and bring the
// cursor back. Guarded so the signal goroutine and the monitor goroutine never
// race on the hook pointer.
var (
	terminalRestoreMu sync.Mutex
	terminalRestore   func()
)

func setTerminalRestore(fn func()) {
	terminalRestoreMu.Lock()
	terminalRestore = fn
	terminalRestoreMu.Unlock()
}

func clearTerminalRestore() {
	setTerminalRestore(nil)
}

// restoreTerminal runs the active TUI restore hook; no-op if the TUI never
// started (piped runs, -no-tui).
func restoreTerminal() {
	terminalRestoreMu.Lock()
	fn := terminalRestore
	terminalRestoreMu.Unlock()
	if fn != nil {
		fn()
	}
}

// exitWithCode restores the terminal before exiting so we never leave the user
// in an alt-screen with a hidden cursor.
func exitWithCode(code int) {
	restoreTerminal()
	os.Exit(code)
}
