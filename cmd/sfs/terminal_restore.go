package main

import (
	"os"
	"sync"
)

const ansiResetScroll = "\033[r"

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

// runs the active TUI restore hook, no-op if TUI never started
func restoreTerminal() {
	terminalRestoreMu.Lock()
	fn := terminalRestore
	terminalRestoreMu.Unlock()
	if fn != nil {
		fn()
	}
}

func exitWithCode(code int) {
	restoreTerminal()
	os.Exit(code)
}
