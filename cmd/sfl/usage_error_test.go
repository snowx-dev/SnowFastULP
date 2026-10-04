package main

import (
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/testutil"
)

// Two positional inputs is the arity error; output stays short.
func TestUsageErrorArityIsShort(t *testing.T) {
	r := newExitRun(t)
	stderr, _, code := r.runSandboxed(t, "first", "second")
	testutil.AssertShortUsageError(t, stderr, code, "expected exactly one input path")
}

// An undefined flag stays short (the flag package calls the short flag.Usage).
func TestUsageErrorBadFlagIsShort(t *testing.T) {
	r := newExitRun(t)
	stderr, _, code := r.runSandboxed(t, "-bogus-flag", "input")
	testutil.AssertShortUsageError(t, stderr, code, "flag provided but not defined")
}

// Explicit -h still prints the full help.
func TestHelpStillFullDump(t *testing.T) {
	r := newExitRun(t)
	_, stdout, code := r.runSandboxed(t, "-h")
	if code != 0 {
		t.Fatalf("-h exit = %d, want 0", code)
	}
	for _, want := range []string{"Usage:", "Args:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("-h output missing %q:\n%s", want, stdout)
		}
	}
}
