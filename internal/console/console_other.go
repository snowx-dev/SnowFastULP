//go:build !windows

package console

// Unix terminals process ANSI natively, so VT is always available.
func platformEnableVT() bool { return true }
