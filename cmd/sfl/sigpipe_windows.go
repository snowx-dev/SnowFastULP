//go:build windows

package main

// ignoreSIGPIPE is a no-op on Windows: there is no SIGPIPE, and writes to a
// closed pipe fail with ERROR_BROKEN_PIPE (reported as EPIPE) directly, which
// tuistat.Emitter already treats as a dead side stream.
func ignoreSIGPIPE() {}
