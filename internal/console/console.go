// Package console handles per-OS terminal setup at process start.
package console

// EnableVT enables ANSI escape processing where supported (Windows) and reports
// whether stderr — the live-TUI target — can render VT sequences. It returns
// false only on a legacy Windows console that ignores the VT bit, so callers
// fall back to plain output instead of emitting raw escape codes. Non-Windows
// and redirected (non-console) stderr return true; the TTY gate handles those.
func EnableVT() bool { return platformEnableVT() }
