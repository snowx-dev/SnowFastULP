package main

import (
	"bytes"
	"io"
	"testing"
)

func TestViewportStdoutDelegatesWhenDisabled(t *testing.T) {
	var buf bytes.Buffer
	layout := &terminalLayout{}
	w := newHitViewportWriter(&buf, layout)
	if _, err := io.WriteString(w, "hit\n"); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "hit\n" {
		t.Fatalf("got %q", got)
	}
}

func TestTerminalLayoutReservedTop(t *testing.T) {
	layout := &terminalLayout{}
	layout.SetReservedTop(0)
	layout.mu.Lock()
	got := layout.reservedTop
	layout.mu.Unlock()
	if got != 1 {
		t.Fatalf("reservedTop = %d, want 1", got)
	}
}
