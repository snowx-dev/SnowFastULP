package main

import "sync"

// terminalRestore holds the live frame's teardown so the signal/force-exit
// goroutine and the panic recovery can leave the alt-screen and restore the
// cursor through the same mutex-guarded path the monitor uses — never a racing
// direct write to stdout. Guarded so the hook pointer can't be torn between
// goroutines.
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

// restoreTerminal runs the active restore hook; no-op when the TUI never
// started (piped runs, -no-tui) or after it has been cleared.
func restoreTerminal() {
	terminalRestoreMu.Lock()
	fn := terminalRestore
	terminalRestoreMu.Unlock()
	if fn != nil {
		fn()
	}
}
