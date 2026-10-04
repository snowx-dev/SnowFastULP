package sflog

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEngineCredFile writes a parseable password file under root.
func writeEngineCredFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A panicking Engine Debug hook must surface as a run error naming the
// callback, not kill the process: workers stop and the caller (cmd/sfl) sees a
// non-nil Run error, which keeps every source out of -del.
func TestDebugCallbackPanicReturnsRunError(t *testing.T) {
	root := t.TempDir()
	writeEngineCredFile(t, root, "Passwords.txt", "URL: a.com\nUSER: u\nPASS: p\n")

	var out bytes.Buffer
	eng := &Engine{Workers: 2, Passwords: []string{""}, Debug: func(string, ...any) { panic("boom-debug") }}
	_, results, err := eng.Run(context.Background(), root, &out)
	if err == nil || !strings.Contains(err.Error(), "sflog: debug callback panic") || !strings.Contains(err.Error(), "boom-debug") {
		t.Fatalf("Run err = %v, want wrapped debug callback panic run error", err)
	}
	if len(results) > 0 && results[0].OK {
		t.Fatalf("results = %+v; a callback-panic run must not look -del eligible", results)
	}
}

// A panicking Engine OnIssue hook (fired from a worker goroutine via the issue
// tee) must convert to a run error instead of crashing the worker.
func TestOnIssueCallbackPanicReturnsRunError(t *testing.T) {
	root := t.TempDir()
	// No complete url:user:pass triple -> IssueNoULP -> OnIssue fires.
	writeEngineCredFile(t, root, "Passwords.txt", "Browser: Chrome\nProfile: Default\n")

	var out bytes.Buffer
	eng := &Engine{
		Workers: 1,
		OnIssue: func(string, IssueKind, error) { panic("boom-issue") },
	}
	_, _, err := eng.Run(context.Background(), root, &out)
	if err == nil || !strings.Contains(err.Error(), "sflog: issue callback panic") || !strings.Contains(err.Error(), "boom-issue") {
		t.Fatalf("Run err = %v, want wrapped issue callback panic run error", err)
	}
}

// A panicking EnvCopier error handler (invoked from the copier and engine
// workers) must record a copier callback error that Engine.Run propagates as a
// run error instead of crashing.
func TestEnvCopierHandlerPanicReturnsRunError(t *testing.T) {
	root := t.TempDir()
	// A symlinked .env fails EnqueueFile's regular-file pre-check, which
	// synchronously reports through the error handler on the engine worker.
	if err := os.Symlink(filepath.Join(root, "missing-target"), filepath.Join(root, ".env")); err != nil {
		t.Fatal(err)
	}

	copier := NewEnvCopier(filepath.Join(t.TempDir(), "env-dest"), nil, EnvCopyMaxLen)
	copier.SetErrorHandler(func(EnvCopyIssue) { panic("boom-handler") })

	var out bytes.Buffer
	eng := &Engine{Workers: 1, Passwords: []string{""}, EnvCopier: copier}
	_, _, err := eng.Run(context.Background(), root, &out)
	if err == nil || !strings.Contains(err.Error(), "sflog: error-handler callback panic") || !strings.Contains(err.Error(), "boom-handler") {
		t.Fatalf("Run err = %v, want wrapped error-handler callback panic run error", err)
	}
}
