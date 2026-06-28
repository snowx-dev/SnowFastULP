package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression for the re-ingest straggler bug. A library archive can hold
// strict-only creds (truncated JSON/cookie password tails). The old regen used
// the loose parser, whose isLikelyJunk gate dropped those lines, so their keys
// were absent from the rebuilt sidecar. Re-ingesting the same source then
// re-emitted them as "new uniques" (stragglers).
//
// With union regen the rebuilt index covers every strict-parseable line, so a
// re-ingest of the archive's own contents dedups to zero uniques.
func TestODUnionRegenNoStragglers(t *testing.T) {
	libDir := t.TempDir()

	// hand-plant an archive with no sidecar -> forces a regen on the next run.
	// 2 plain creds + 2 strict-only (open-brace, no closing brace so they pass
	// strict's wrappedBraces check but trip loose's isLikelyJunk gate).
	lines := []string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
		`twitter.com:moraxd5:{"uid":"7178515064324310021","token"`,
		`dash.cloudflare.com/sign-up:holik@gmail.com:{"cc"`,
	}
	pastArchive := filepath.Join(libDir, "sfu_old.txt.zst")
	writeZstdArchive(t, pastArchive, lines)

	// sanity: every planted line is strict-parseable (so re-ingest produces a
	// key for it), and the two json-tail lines are exactly the strict-only set
	// loose would drop.
	strictOnly := 0
	for _, ln := range lines {
		if _, _, _, _, ok := parse(ln); !ok {
			t.Fatalf("test setup: strict rejected planted line %q", ln)
		}
		if _, _, _, _, ok := parseLoose(ln); !ok {
			strictOnly++
		}
	}
	if strictOnly != 2 {
		t.Fatalf("test setup: want 2 strict-only lines, got %d", strictOnly)
	}

	// re-ingest the archive's own contents.
	reInput := filepath.Join(t.TempDir(), "reingest.txt")
	writeFileContent(t, reInput, strings.Join(lines, "\n")+"\n")

	r, err := Resolve(Config{
		Inputs:       []string{reInput},
		Output:       filepath.Join(libDir, "sfu_new.txt.zst"),
		TempDir:      filepath.Join(libDir, ".stage"),
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     "new",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	m := &Metrics{}
	if err := Run(context.Background(), &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      filepath.Join(libDir, ".stage"),
	}, m); err != nil {
		t.Fatalf("run: %v", err)
	}

	// the whole input already lives in the library -> zero stragglers.
	if got := m.LinesUnique.Load(); got != 0 {
		t.Errorf("LinesUnique = %d, want 0 (union regen must index strict-only creds)", got)
	}
	if got := m.LinesSkippedByDest.Load(); got != int64(len(lines)) {
		t.Errorf("LinesSkippedByDest = %d, want %d", got, len(lines))
	}

	// regenerated sidecar must carry a key for every distinct line, including
	// the two strict-only ones loose would have dropped.
	hdr, err := readSidecarHeader(sidecarPathForArchive(pastArchive))
	if err != nil {
		t.Fatalf("regenerated sidecar: %v", err)
	}
	if hdr.keyCount != uint64(len(lines)) {
		t.Errorf("regenerated sidecar keyCount = %d, want %d (strict-only creds dropped?)",
			hdr.keyCount, len(lines))
	}
}

// End-to-end round-trip guard: a library's own output must re-ingest with zero
// stragglers. An LPU line whose URL embeds colons has no stored form that
// re-parses to its key, so the guard drops it at write time (counted). Whatever
// the library does write must therefore survive a sidecar regen with no
// leftover uniques.
func TestRoundTripGuardSelfReingestNoStragglers(t *testing.T) {
	libDir := t.TempDir()

	run1Input := filepath.Join(t.TempDir(), "in1.txt")
	writeFileContent(t, run1Input, strings.Join([]string{
		"https://a.example.com:user1:pw1",
		"https://b.example.com:user2:pw2",
		`twitter.com:moraxd5:{"uid":"123","token"`, // strict-only but round-trippable -> kept
		`jurbzdm:astr.m@ou4eudeaeC:Estr@6438:https://om.fhttpiip-dual/:abdell.zouad@gmail.co:NellaAde9:@Nv@g`, // unrepresentable -> dropped
	}, "\n")+"\n")

	m1 := runBucketedIngest(t, libDir, run1Input, "one")
	if got := m1.LinesUnrepresentable.Load(); got != 1 {
		t.Fatalf("run1 LinesUnrepresentable = %d, want 1 (colon-url LPU line should be dropped)", got)
	}
	if got := m1.LinesRejected.Load(); got != 1 {
		t.Errorf("run1 LinesRejected = %d, want 1 (drop folds into rejected)", got)
	}

	// part1's stored lines: the guard already excluded the unrepresentable one.
	stored := readZstdLines(t, filepath.Join(libDir, "sfu_one.txt.zst"))
	if len(stored) != 3 {
		t.Fatalf("part1 stored %d lines, want 3 (unrepresentable line must not be written)", len(stored))
	}

	// drop the sidecar to force a union regen of part1 on the next run.
	if err := os.RemoveAll(filepath.Join(libDir, idxSubdirName)); err != nil {
		t.Fatal(err)
	}

	// re-ingest part1's own contents -> every stored line must dedup.
	run2Input := filepath.Join(t.TempDir(), "in2.txt")
	writeFileContent(t, run2Input, strings.Join(stored, "\n")+"\n")
	m2 := runBucketedIngest(t, libDir, run2Input, "two")
	if got := m2.LinesUnique.Load(); got != 0 {
		t.Errorf("run2 LinesUnique = %d, want 0 (library self-reingest must not straggle)", got)
	}
	if got := m2.LinesSkippedByDest.Load(); got != int64(len(stored)) {
		t.Errorf("run2 LinesSkippedByDest = %d, want %d", got, len(stored))
	}
}

// runBucketedIngest runs one -od ingest of input into libDir, tagged by stamp,
// and returns its metrics. Mirrors the bucketed-path setup used across the -od
// tests.
func runBucketedIngest(t *testing.T, libDir, input, stamp string) *Metrics {
	t.Helper()
	stage := filepath.Join(libDir, ".stage_"+stamp)
	r, err := Resolve(Config{
		Inputs:       []string{input},
		Output:       filepath.Join(libDir, "sfu_"+stamp+".txt.zst"),
		TempDir:      stage,
		FastPathOff:  true,
		Buckets:      4,
		Compress:     true,
		DestDedup:    true,
		DestDedupDir: libDir,
		RunStamp:     stamp,
	})
	if err != nil {
		t.Fatalf("resolve %s: %v", stamp, err)
	}
	m := &Metrics{}
	if err := Run(context.Background(), &Resolved{
		Cfg:          r.Cfg,
		TotalInputs:  r.TotalInputs,
		mem:          r.mem,
		BucketCount:  4,
		Workers:      1,
		DedupWorkers: 1,
		chunkBytes:   1 << 20,
		TempDir:      stage,
	}, m); err != nil {
		t.Fatalf("run %s: %v", stamp, err)
	}
	return m
}
