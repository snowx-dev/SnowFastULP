//go:build !unix

package main

import "os"

func notifyTerminalResize(_ chan<- os.Signal) {}
