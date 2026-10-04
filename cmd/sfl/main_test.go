package main

import (
	"fmt"
	"os"
	"testing"
)

// TestMain sandboxes the whole package's temp directory: tests that record
// issues (or otherwise touch the platform temp root) must never leak
// sfl-issues-*.log files into the user's real TMPDIR. Individual tests may
// still narrow it further with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sfl-test-tmp")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sfl test setup:", err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
