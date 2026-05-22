//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func notifyTerminalResize(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGWINCH)
}
