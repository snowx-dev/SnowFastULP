package sflog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const t06AmbiguousLine = "https://example.com/login:alice:pw:tail"

func runT06DiagnosticEngine(t *testing.T, workers int, input string) (int, int, string) {
	t.Helper()
	engine := &Engine{Workers: workers, TempDir: t.TempDir(), Debug: func(string, ...any) {}}
	var output bytes.Buffer
	stats, _, err := engine.Run(context.Background(), input, &output)
	if err != nil {
		t.Fatalf("Engine.Run workers=%d: %v", workers, err)
	}
	return stats.AmbiguityTotal, stats.AmbiguityPathOrPassword, output.String()
}

func TestEngineAmbiguityCountsAgreeAcrossWorkersAndNestedParallelArchives(t *testing.T) {
	plainDir := t.TempDir()
	for i := range 20 {
		path := filepath.Join(plainDir, fmt.Sprintf("victim%02d", i), "Passwords.txt")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		body := strings.Repeat(t06AmbiguousLine+"\n", 3)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, workers := range []int{1, 4} {
		total, pathCount, _ := runT06DiagnosticEngine(t, workers, plainDir)
		if total != 60 || pathCount != 60 {
			t.Fatalf("plain workers=%d ambiguity=(%d,%d), want (60,60)", workers, total, pathCount)
		}
	}

	inner := zipBytes(t, map[string][]byte{"nested/Passwords.txt": []byte(t06AmbiguousLine + "\n")})
	members := map[string][]byte{"nested/inner.zip": inner}
	for i := range 50 {
		members[fmt.Sprintf("victim%03d/Passwords.txt", i)] = []byte(t06AmbiguousLine + "\n")
	}
	archive := filepath.Join(t.TempDir(), "parallel-nested.zip")
	writeZipMembers(t, archive, members)
	var baselineOutput string
	for _, workers := range []int{1, 4} {
		total, pathCount, output := runT06DiagnosticEngine(t, workers, archive)
		if total != 51 || pathCount != 51 {
			t.Fatalf("archive workers=%d ambiguity=(%d,%d), want (51,51)", workers, total, pathCount)
		}
		if workers == 1 {
			baselineOutput = output
		} else if output != baselineOutput {
			t.Fatalf("parallel archive output differs from sequential output")
		}
	}
}

func TestEngineCancelledAmbiguityCountsAreAbsentOrComplete(t *testing.T) {
	root := t.TempDir()
	badDir := filepath.Join(root, "00-bad")
	goodDir := filepath.Join(root, "01-good")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(goodDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(badDir, "Passwords.txt")
	if err := os.WriteFile(bad, []byte(strings.Repeat("x", maxScanLineLen+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	const completeCount = 2000
	var body strings.Builder
	for i := range completeCount {
		fmt.Fprintf(&body, "https://example.com/login:user%04d:pw:tail\n", i)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "Passwords.txt"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine := &Engine{
		Workers: 2,
		TempDir: t.TempDir(),
		Debug: func(format string, _ ...any) {
			if strings.Contains(format, "parse error") {
				cancel()
			}
		},
	}
	stats, _, err := engine.Run(ctx, root, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run error = %v, want context.Canceled", err)
	}
	if stats.AmbiguityTotal != 0 && stats.AmbiguityTotal != completeCount {
		t.Fatalf("cancelled run ambiguity count = %d, want absent (0) or complete source (%d)", stats.AmbiguityTotal, completeCount)
	}
	if stats.AmbiguityPathOrPassword != stats.AmbiguityTotal {
		t.Fatalf("cancelled run category count=%d total=%d; counts are inconsistent", stats.AmbiguityPathOrPassword, stats.AmbiguityTotal)
	}
}

func TestEngineCancelledAmbiguityStateDoesNotLeakAcrossConcurrentRuns(t *testing.T) {
	cancelledDir, completeDir := t.TempDir(), t.TempDir()
	cancelledInput := filepath.Join(cancelledDir, "Passwords.txt")
	completeInput := filepath.Join(completeDir, "Passwords.txt")
	if err := os.WriteFile(cancelledInput, []byte(strings.Repeat(t06AmbiguousLine+"\n", 2000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(completeInput, []byte(strings.Repeat(t06AmbiguousLine+"\n", 37)), 0o600); err != nil {
		t.Fatal(err)
	}
	cancelledTempDir, completeTempDir := t.TempDir(), t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	type result struct {
		total int
		err   error
	}
	cancelled := make(chan result, 1)
	complete := make(chan result, 1)
	go func() {
		engine := &Engine{Workers: 4, TempDir: cancelledTempDir, Debug: func(string, ...any) {}}
		stats, _, err := engine.Run(ctx, cancelledInput, &bytes.Buffer{})
		cancelled <- result{total: stats.AmbiguityTotal, err: err}
	}()
	go func() {
		engine := &Engine{Workers: 4, TempDir: completeTempDir, Debug: func(string, ...any) {}}
		stats, _, err := engine.Run(context.Background(), completeInput, &bytes.Buffer{})
		complete <- result{total: stats.AmbiguityTotal, err: err}
	}()
	cancelledResult := <-cancelled
	completeResult := <-complete
	if !errors.Is(cancelledResult.err, context.Canceled) || cancelledResult.total != 0 {
		t.Fatalf("pre-cancelled run result = total %d, err %v; want no counts and context.Canceled", cancelledResult.total, cancelledResult.err)
	}
	if completeResult.err != nil {
		t.Fatalf("independent complete run failed: %v", completeResult.err)
	}
	if completeResult.total != 37 {
		t.Fatalf("independent complete run ambiguity total = %d, want 37", completeResult.total)
	}

	// A subsequent run starts with fresh diagnostics state; the prior cancelled
	// run and its concurrent peer cannot contribute to its summary.
	repeated, _, _ := runT06DiagnosticEngine(t, 1, completeInput)
	if repeated != 37 {
		t.Fatalf("subsequent independent run ambiguity total = %d, want 37", repeated)
	}
}
