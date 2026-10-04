package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
)

// M-11: deterministic bad invocations are argv-shape errors and must use the
// usage exit code (2), not the runtime error code (1). SFS already routes its
// JSON interval through the usage path; sfu/sfl used to exit 1.
func TestUsageExitCodeJSONIntervalSFU(t *testing.T) {
	dir := t.TempDir()
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-json", filepath.Join(dir, "stats.jsonl"), "-json-every", "0",
		sfuSpaceFormInput(t, dir))
	if code != exitcode.Usage {
		t.Fatalf("sfu -json-every 0 exit = %d, want %d (usage)\nstderr:\n%s", code, exitcode.Usage, stderr)
	}
}

func TestUsageExitCodeParseDelimsSFU(t *testing.T) {
	dir := t.TempDir()
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-parse-delims", ":",
		sfuSpaceFormInput(t, dir))
	if code != exitcode.Usage {
		t.Fatalf("sfu -parse-delims exit = %d, want %d (usage)\nstderr:\n%s", code, exitcode.Usage, stderr)
	}
}

// M-11 defect (re-review): the newJSONOut error site was converted to usagef
// wholesale, so an ENVIRONMENTAL failure — the stream file cannot be opened —
// wrongly exits 2 with the usage hint. Only genuine target-collision errors
// are usage errors; an open failure is a runtime error (1, no usage hint).
func TestJSONOutEnvironmentalOpenFailureExitsOne(t *testing.T) {
	dir := t.TempDir()
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check",
		"-json", filepath.Join(dir, "nonexistent-dir-xyz", "s.jsonl"),
		"-o", filepath.Join(dir, "out.d")+string(filepath.Separator),
		sfuSpaceFormInput(t, dir))
	if code != exitcode.Error {
		t.Fatalf("sfu -json into a missing directory: exit = %d, want %d (runtime)\nstderr:\n%s", code, exitcode.Error, stderr)
	}
	if strings.Contains(stderr, "-h for help") {
		t.Fatalf("environmental failure must not print the usage hint:\n%s", stderr)
	}
}

// M-11 defect (re-review): -parse-delims together with -parse-rules is a
// mutually exclusive invocation error and must use the usage code (2), not
// the runtime code (1).
func TestUsageExitCodeParseDelimsRulesMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	stderr, code := runSFUSpaceForm(t, dir, "",
		"-no-tui", "-no-update-check", "-parse-delims", ":", "-parse-rules", filepath.Join(dir, "rules.conf"),
		sfuSpaceFormInput(t, dir))
	if code != exitcode.Usage {
		t.Fatalf("sfu -parse-delims + -parse-rules exit = %d, want %d (usage)\nstderr:\n%s", code, exitcode.Usage, stderr)
	}
}
