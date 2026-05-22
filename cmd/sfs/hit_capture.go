package main

import (
	"io"
	"os"
)

// buffers stdout hits for replay after alt-screen exits
type hitCapture struct {
	inner io.Writer
	buf   []byte
}

func newHitCapture(inner io.Writer) *hitCapture {
	return &hitCapture{inner: inner}
}

func (c *hitCapture) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.buf = append(c.buf, p...)
	}
	if c.inner == nil {
		return len(p), nil
	}
	return c.inner.Write(p)
}

func (c *hitCapture) ReplayToStdout() {
	if c == nil || len(c.buf) == 0 {
		return
	}
	_, _ = os.Stdout.Write(c.buf)
}
