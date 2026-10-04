package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/exitcode"
)

// Repair, review finding 1 (HIGH): an isolated source whose only labeled
// credentials fail the dual-fidelity emission gate (stored decoding accepts,
// the raw consumer used by DedupKeyForLine and ingest rejects) must emit
// nothing, count as ordinary quality rejects, and return the existing
// NothingUsable exit 4 — for both the classic output path and the -od library
// ingest path, which must agree. History is left disabled and the environment
// is fully sandboxed (empty SNOWFAST config, update checks off), as in the
// other cmd/sfl e2e tests.
func TestRepairAllRejectedLooseFixtureExits4(t *testing.T) {
	body := "URL: example.com\nUSER: $alice\nPASS: a:b\n\n" +
		"URL: example.com\nUSER: Zoë\nPASS: a:b\n\n"

	run := func(od string) int {
		r := newExitRun(t)
		if od != "" {
			r.out = "" // -od replaces -o; the two flags are mutually exclusive
		}
		input := filepath.Join(r.dir, "Passwords.txt")
		if err := os.WriteFile(input, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		args := []string{"-loose", input}
		if od != "" {
			args = []string{"-loose", "-od", od, input}
		}
		stderr, stdout, code := r.runSandboxed(t, args...)
		if strings.Contains(stdout, "example.com:") {
			t.Fatalf("%s run emitted defect tuples on stdout:\n%s", odOrClassic(od), stdout)
		}
		if code != exitcode.NothingUsable {
			t.Fatalf("%s all-rejected run exit = %d, want %d\nstderr:\n%s",
				odOrClassic(od), code, exitcode.NothingUsable, stderr)
		}
		return code
	}

	run("")
	run(filepath.Join(t.TempDir(), "lib") + string(os.PathSeparator))
}

func odOrClassic(od string) string {
	if od != "" {
		return "-od"
	}
	return "classic"
}

// Repair, lane gate: a strict-lane fixture still emits end-to-end, and the
// classic output path and the -od library ingest path agree on the emitted
// lines — the lane gate that skips the raw-consumer recheck for strict-lane
// admissions changes no observable output.
func TestRepairStrictLaneFixtureEmitsClassicAndODAgree(t *testing.T) {
	body := "URL: example.com\nUSER: alice\nPASS: pw\n\n" +
		"URL: https://example.com/oauth:callback\nUSER: bob\nPASS: s3cret\n\n"
	want := []string{"example.com:alice:pw", "example.com:bob:s3cret"}

	check := func(od string) []string {
		r := newExitRun(t)
		if od != "" {
			// -o and -od are mutually exclusive; drop the harness default.
			r.out = ""
		}
		input := filepath.Join(r.dir, "Passwords.txt")
		if err := os.WriteFile(input, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		args := []string{input}
		if od != "" {
			args = []string{"-od", od, input}
		}
		stderr, _, code := r.runSandboxed(t, args...)
		if code != exitcode.Clean {
			t.Fatalf("%s strict-lane run exit = %d, want %d\nstderr:\n%s",
				odOrClassic(od), code, exitcode.Clean, stderr)
		}
		var lines []string
		if od == "" {
			raw := readFileString(t, globOne(t, filepath.Join(r.out, "sfl_*.txt")))
			lines = strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
		} else {
			// -od ingests into an sfu library; read every archive back.
			lines = libLines(t, od)
		}
		if len(lines) != len(want) {
			t.Fatalf("%s emitted %d line(s): %q; want %q", odOrClassic(od), len(lines), lines, want)
		}
		got := append([]string(nil), lines...)
		wantSorted := append([]string(nil), want...)
		sort.Strings(got)
		sort.Strings(wantSorted)
		for i := range wantSorted {
			if got[i] != wantSorted[i] {
				t.Fatalf("%s line %d = %q, want %q (full: %q)", odOrClassic(od), i, got[i], wantSorted[i], lines)
			}
		}
		return lines
	}

	classic := check("")
	od := filepath.Join(t.TempDir(), "lib") + string(os.PathSeparator)
	lib := check(od)
	if len(classic) != len(lib) {
		t.Fatalf("classic emitted %d line(s), -od library %d", len(classic), len(lib))
	}
	sort.Strings(classic)
	sort.Strings(lib)
	for i := range classic {
		if classic[i] != lib[i] {
			t.Fatalf("classic and -od disagree at sorted line %d: %q vs %q", i, classic[i], lib[i])
		}
	}
}
