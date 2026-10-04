package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// L-03: the schema defines library.added as the actual or would-be additions,
// but the summary set it only when !DryRun, so a dry run reported the unique
// count while omitting the would-add count the -odr preview exists to show.
func TestSFUJSONOutDryRunSummaryLibraryAdded(t *testing.T) {
	bin := buildSFUE2E(t)
	dir := t.TempDir()
	input := writeHistoryInput(t, dir, "input.txt", "https://a.example.com:alice:pw123\n")
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o700); err != nil {
		t.Fatal(err)
	}

	runJSON := func(t *testing.T, extra ...string) map[string]any {
		t.Helper()
		jsonl := filepath.Join(dir, t.Name()+"-stats.jsonl")
		args := append(extra, "-json", jsonl)
		runSFUE2E(t, bin, dir, input, args...)
		snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
		last := snaps[len(snaps)-1]
		sum, ok := last["summary"].(map[string]any)
		if !ok {
			t.Fatalf("summary block missing: %v", last)
		}
		return sum
	}

	// Empty library, dry run: the one unique line is a pure would-add.
	sum := runJSON(t, "-odr", lib)
	summaryLib, ok := sum["library"].(map[string]any)
	if !ok {
		t.Fatalf("dry-run summary must carry a library block: %v", sum)
	}
	if added, ok := summaryLib["added"]; !ok || added != float64(1) {
		t.Fatalf("dry-run library = %v, want added=1 (empty library, one unique line)", summaryLib)
	}
	// An empty pre-run library keeps the unchanged pre-run total out.
	if lines, ok := summaryLib["lines"]; ok && lines != float64(0) {
		t.Fatalf("dry-run library lines = %v, want omitted (empty pre-run library)", summaryLib)
	}

	// Populated library: seed one line for real, then dry-run a second.
	runSFUE2E(t, bin, dir, input, "-od", lib)
	input2 := writeHistoryInput(t, dir, "input2.txt", "https://b.example.com:bob:pw456\n")
	jsonl := filepath.Join(dir, t.Name()+"-seeded-stats.jsonl")
	runSFUE2E(t, bin, dir, input2, "-odr", lib, "-json", jsonl)
	snaps := decodeSfuJSONL(t, mustRead(t, jsonl))
	last := snaps[len(snaps)-1]
	sum, ok = last["summary"].(map[string]any)
	if !ok {
		t.Fatalf("seeded dry-run summary block missing: %v", last)
	}
	summaryLib, ok = sum["library"].(map[string]any)
	if !ok {
		t.Fatalf("seeded dry-run summary must carry a library block: %v", sum)
	}
	if added, ok := summaryLib["added"]; !ok || added != float64(1) {
		t.Fatalf("seeded dry-run library = %v, want added=1", summaryLib)
	}
	if lines, ok := summaryLib["lines"]; !ok || lines != float64(1) {
		t.Fatalf("seeded dry-run library = %v, want lines=1 (pre-run total, unchanged)", summaryLib)
	}
}

// The non-dry-run mapping is unchanged: Added stays the lines actually added.
func TestSFUJSONOutSummaryLibraryAddedNonDryRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dryRun bool
	}{
		{"dry_run", true},
		{"actual", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &ulpengine.Metrics{}
			m.LinesUnique.Store(3)
			r := &ulpengine.Resolved{
				OdResult: &ulpengine.ODResult{TotalKeysLoaded: 10},
				Cfg:      ulpengine.Config{DryRun: tc.dryRun},
			}
			j := &jsonOut{}
			j.setEngine(m, r)
			b := j.summaryBlock()
			if b.Library == nil || b.Library.Added != 3 {
				t.Fatalf("library = %+v, want added=3", b.Library)
			}
			if tc.dryRun && b.Library.Lines != 10 {
				t.Fatalf("dry-run library lines = %d, want the pre-run total 10", b.Library.Lines)
			}
		})
	}
}
