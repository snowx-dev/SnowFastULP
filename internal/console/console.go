// Package console handles per-OS terminal setup at process start.
package console

// EnableVT enables ANSI escape processing where supported (Windows). no-op on Unix.
func EnableVT() { platformEnableVT() }
