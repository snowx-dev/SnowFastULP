//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
)

// M-11: deterministic bad invocations are argv-shape errors and must use the
// usage exit code (2), not the runtime error code (1). SFS already routes its
// JSON interval through the usage path; sfl used to exit 1.
func TestUsageExitCodeJSONIntervalSFL(t *testing.T) {
	dir, input, out, jsonl := sflSpaceFormFixture(t)
	stderr, code := runSFLSpaceForm(t, dir,
		"-config", filepath.Join(dir, "empty.toml"), "-no-tui", "-no-update-check",
		"-json", jsonl, "-json-every", "0", "-o", out+string(os.PathSeparator), input)
	if code != exitcode.Usage {
		t.Fatalf("sfl -json-every 0 exit = %d, want %d (usage)\nstderr:\n%s", code, exitcode.Usage, stderr)
	}
}
