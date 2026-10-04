//go:build unix

package main

import (
	"os/signal"
	"syscall"
)

// ignoreSIGPIPE makes writes to a broken pipe return EPIPE instead of
// terminating the process. Used only for the -json stdout target: the JSON
// stream is an optional side channel, so an early consumer exit (e.g.
// `sfl ... -json=- | head -1`) must reach tuistat.Emitter's write path as
// an EPIPE — which marks the stream dead and lets the run finish with all
// defers (terminal restore, summary, staging cleanup) — instead of the
// default SIGPIPE kill, which aborts sfl mid-run.
func ignoreSIGPIPE() {
	signal.Ignore(syscall.SIGPIPE)
}
