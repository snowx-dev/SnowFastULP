package main

import (
	"bytes"
	"testing"
)

func TestHitCaptureReplay(t *testing.T) {
	var buf bytes.Buffer
	c := newHitCapture(&buf)
	if _, err := c.Write([]byte("line1\nline2\n")); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "line1\nline2\n" {
		t.Fatalf("inner write = %q", got)
	}
	if len(c.buf) != len("line1\nline2\n") {
		t.Fatalf("capture len = %d", len(c.buf))
	}
}

func TestHitCaptureNilInner(t *testing.T) {
	c := newHitCapture(nil)
	if _, err := c.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if string(c.buf) != "x\n" {
		t.Fatalf("buf = %q", c.buf)
	}
}
