// Package testutil holds cross-binary test assertions.
package testutil

import (
	"strings"
	"testing"
)

// AssertShortUsageError asserts the D4 usage-error contract shared by sfl,
// sfu, and sfs: a trivial usage error exits 2 and prints the one-line error
// plus a single hint line, never the full help dump (which stays on -h).
func AssertShortUsageError(t *testing.T, stderr string, code int, wantErr string) {
	t.Helper()
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, wantErr) {
		t.Fatalf("stderr missing %q:\n%s", wantErr, stderr)
	}
	if !strings.Contains(stderr, "run with -h for help") {
		t.Fatalf("stderr missing the hint line:\n%s", stderr)
	}
	if lines := strings.Count(strings.TrimRight(stderr, "\n"), "\n") + 1; lines > 3 {
		t.Fatalf("stderr has %d lines, want a short error (one line + hint):\n%s", lines, stderr)
	}
	for _, banned := range []string{"Usage:", "Args (for nerds):", "Examples:"} {
		if strings.Contains(stderr, banned) {
			t.Fatalf("stderr must not dump the help text (%q found):\n%s", banned, stderr)
		}
	}
}
