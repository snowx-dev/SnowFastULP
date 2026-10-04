package main

import (
	"strings"
	"testing"
)

func TestRenderInterruptShowsCleanupLog(t *testing.T) {
	log := []string{"removed /tmp/sfs_results.txt"}
	joined := strings.Join(renderInterrupt(0, 80, log), "\n")
	for _, want := range []string{"INTERRUPTED", "removed /tmp/sfs_results.txt", "Ctrl+C"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("interrupt frame missing %q:\n%s", want, joined)
		}
	}
}
