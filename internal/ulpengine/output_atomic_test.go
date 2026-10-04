package ulpengine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sentinel content planted in a pre-existing output; a failed or cancelled run
// must never truncate or remove it.
const atomicSentinel = "PRE-EXISTING GOOD OUTPUT BYTES\n"

func writeAtomicSentinel(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(atomicSentinel), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAtomicFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// tempLeftovers lists files in dir whose name matches the staged-temp pattern
// ("." + base + ".tmp-*"), i.e. anything the run forgot to clean up.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), ".tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func assertNoTempLeftovers(t *testing.T, dir string) {
	t.Helper()
	if left := tempLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("staged temp files left behind: %v", left)
	}
}

// TestOutputAtomicFailurePreservesSentinel: a run that fails (or is cancelled)
// after the sink created its staged temp must leave a pre-existing output with
// the same name byte-for-byte intact and must not leave a temp behind.
func TestOutputAtomicFailurePreservesSentinel(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "sfu_x.txt.zst")
	writeAtomicSentinel(t, final)

	sink, err := newOutputSink(final, false, false)
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	if err := writeLine(sink, "https://a.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	// the pipeline failure path: abort the sink, never touch the final name.
	if err := sink.abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got := readAtomicFile(t, final); got != atomicSentinel {
		t.Fatalf("pre-existing output clobbered: got %q", got)
	}
	assertNoTempLeftovers(t, dir)
}

// TestOutputAtomicSealStagesSidecarsCommitPublishes: seal() finalizes the
// archive bytes and stages both sidecar types but publishes nothing; commit()
// publishes archive + .idx + search sidecar in one go.
func TestOutputAtomicSealStagesSidecarsCommitPublishes(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "sfu_y.txt.zst")

	sink, err := newOutputSinkWithSidecar(final, true, true)
	if err != nil {
		t.Fatalf("new sink: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := writeLine(sink, "https://b.example.com:u:p", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}

	// nothing published at seal time
	for _, p := range []string{
		final,
		sidecarPathForArchive(final),
		searchSidecarPathForArchive(final),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s published before commit: stat err=%v", filepath.Base(p), err)
		}
	}
	// staged archive temp exists in the same directory
	if left := tempLeftovers(t, dir); len(left) != 1 {
		t.Fatalf("want exactly 1 staged archive temp at seal, got %v", left)
	}

	if err := sink.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertNoTempLeftovers(t, dir)

	if fi, err := os.Stat(final); err != nil || fi.Size() == 0 {
		t.Fatalf("committed archive missing/empty: %v", err)
	}
	if _, err := os.Stat(sidecarPathForArchive(final)); err != nil {
		t.Fatalf("committed .idx missing: %v", err)
	}
	sc := readAtomicFile(t, searchSidecarPathForArchive(final))
	// staging must follow the archive's final basename
	if !strings.Contains(sc, `"source": "sfu_y.txt.zst"`) {
		t.Fatalf("search sidecar source mismatch:\n%s", sc)
	}

	// idempotent commit
	if err := sink.commit(); err != nil {
		t.Fatalf("second commit should be a no-op, got %v", err)
	}
}

// TestOutputAtomicCancelBetweenSealAndCommit: a cancel arriving after seal but
// before commit publishes nothing, removes the staged temps, and leaves a
// pre-existing output intact.
func TestOutputAtomicCancelBetweenSealAndCommit(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "sfu_z.txt.zst")
	writeAtomicSentinel(t, final)

	sink, err := newOutputSink(final, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLine(sink, "https://c.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if got := readAtomicFile(t, final); got != atomicSentinel {
		t.Fatalf("seal published over pre-existing output: got %q", got)
	}
	if err := sink.abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got := readAtomicFile(t, final); got != atomicSentinel {
		t.Fatalf("cancel clobbered pre-existing output: got %q", got)
	}
	assertNoTempLeftovers(t, dir)
}

// TestChunkedFailureAfterPart1Seals: when part 1 is already sealed and the run
// then fails, every pre-existing part file survives byte-for-byte and no
// staged temps remain.
func TestChunkedFailureAfterPart1Seals(t *testing.T) {
	dir := t.TempDir()
	stamp := "20260922_atomic"

	// pre-existing outputs from a prior run
	oldBase := filepath.Join(dir, DefaultBasename(stamp)+".txt.zst")
	oldPart2 := zstPartPath(dir, stamp, 2)
	writeAtomicSentinel(t, oldBase)
	writeAtomicSentinel(t, oldPart2)

	c, err := newChunkedZstdSink(dir, stamp, 1, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLine(c, "https://a.example.com:u:p", nil); err != nil { // part 1
		t.Fatal(err)
	}
	if err := writeLine(c, "https://b.example.com:u:p", nil); err != nil { // rotate: part 1 seals
		t.Fatal(err)
	}
	if err := writeLine(c, "https://c.example.com:u:p", nil); err != nil { // part 2
		t.Fatal(err)
	}
	// mid-run failure: abort everything staged, publish nothing
	if err := c.abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got := readAtomicFile(t, oldBase); got != atomicSentinel {
		t.Fatalf("old base part clobbered: got %q", got)
	}
	if got := readAtomicFile(t, oldPart2); got != atomicSentinel {
		t.Fatalf("old part2 clobbered: got %q", got)
	}
	assertNoTempLeftovers(t, dir)
}

// TestChunkedCommitPublishesAllParts: a successful multi-part run publishes
// every part only at commit, with part 1 under the _part1 name.
func TestChunkedCommitPublishesAllParts(t *testing.T) {
	dir := t.TempDir()
	stamp := "20260922_commit"

	c, err := newChunkedZstdSink(dir, stamp, 1, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"https://a.example.com:u:p", "https://b.example.com:u:p", "https://c.example.com:u:p"} {
		if err := writeLine(c, l, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	assertNoTempLeftovers(t, dir)

	base := filepath.Join(dir, DefaultBasename(stamp)+".txt.zst")
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("multi-part run must not leave the unsuffixed part-1 name: %v", err)
	}
	for i := 1; i <= 3; i++ {
		p := zstPartPath(dir, stamp, i)
		fi, err := os.Stat(p)
		if err != nil || fi.Size() == 0 {
			t.Fatalf("part %d missing/empty: %v", i, err)
		}
	}
}

// TestChunkedMidCommitFailureReportsCommittedParts: if part 2's publication
// fails, part 1 must already be published, the pre-existing part-2 target must
// be untouched, no temps may remain, and the error must say exactly which
// parts were committed.
// A directory parked at a later part's destination makes the WHOLE batch
// refuse before anything publishes (H-22 preflight): no partial archive set,
// the offending entry untouched, the error naming the destination.
func TestChunkedMidCommitFailureRefusesBatchUpFront(t *testing.T) {
	dir := t.TempDir()
	stamp := "20260922_midfail"

	c, err := newChunkedZstdSink(dir, stamp, 1, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLine(c, "https://a.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	if err := writeLine(c, "https://b.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}

	// sabotage part 2's publish target: a directory cannot be overwritten by
	// rename, so the batch refuses up front.
	part2 := zstPartPath(dir, stamp, 2)
	if err := os.MkdirAll(part2, 0o755); err != nil {
		t.Fatal(err)
	}

	err = c.commit()
	if err == nil {
		t.Fatal("commit should have failed on part 2's non-regular destination")
	}
	if !strings.Contains(err.Error(), "not a regular file") || !strings.Contains(err.Error(), filepath.Base(part2)) {
		t.Fatalf("error must name the offending destination: %v", err)
	}

	// nothing was published and nothing was touched
	part1 := zstPartPath(dir, stamp, 1)
	if _, serr := os.Stat(part1); !os.IsNotExist(serr) {
		t.Fatalf("part 1 must not be published when the batch refuses up front: %v", serr)
	}
	if fi, serr := os.Stat(part2); serr != nil || !fi.IsDir() {
		t.Fatalf("pre-existing part-2 target must be untouched: %v", serr)
	}
	assertNoTempLeftovers(t, dir)
}
func TestOutputAtomicLifecycleIdempotent(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "sfu_i.txt")
	sink, err := newOutputSink(final, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLine(sink, "https://d.example.com:u:p", nil); err != nil {
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatal(err)
	}
	if err := sink.seal(); err != nil {
		t.Fatalf("second seal should be a no-op, got %v", err)
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	if err := sink.commit(); err != nil {
		t.Fatalf("second commit should be a no-op, got %v", err)
	}
	if err := sink.abort(); err != nil {
		t.Fatalf("abort after commit should be a no-op, got %v", err)
	}
	if got := readAtomicFile(t, final); !strings.Contains(got, "https://d.example.com:u:p") {
		t.Fatalf("committed content wrong: %q", got)
	}
}

// TestStagedTempsRegisteredUntilCommit: the force-exit cleanup registry tracks
// the staged temp paths until commit; committed finals are never registered.
func TestStagedTempsRegisteredUntilCommit(t *testing.T) {
	t.Cleanup(resetCleanupRegistry)
	resetCleanupRegistry()

	dir := t.TempDir()
	final := filepath.Join(dir, "sfu_r.txt")
	sink, err := newOutputSink(final, false, false)
	if err != nil {
		t.Fatal(err)
	}
	temps := tempLeftovers(t, dir)
	if len(temps) != 1 {
		t.Fatalf("want 1 staged temp, got %v", temps)
	}
	staged := filepath.Join(dir, temps[0])
	registered := map[string]bool{}
	for _, p := range SnapshotCleanupPaths() {
		registered[p] = true
	}
	if !registered[staged] {
		t.Fatalf("staged temp not cleanup-registered: %v", SnapshotCleanupPaths())
	}
	if err := sink.commit(); err != nil {
		t.Fatal(err)
	}
	for _, p := range SnapshotCleanupPaths() {
		if p == staged {
			t.Error("committed temp still cleanup-registered")
		}
		if p == final {
			t.Error("final output must never be a cleanup candidate")
		}
	}
	assertNoTempLeftovers(t, dir)
}

// TestEmptyRunKeepsExistingOutput: a run whose every line is rejected
// publishes nothing; a pre-existing output with the same name survives, on
// both the bucketed and fast paths.
func TestEmptyRunKeepsExistingOutput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fastPath bool
	}{
		{"bucketed", false},
		{"fastpath", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			in := filepath.Join(d, "in.txt")
			writeFile(t, in, "not-a-line\nalso not valid\n\n")
			out := filepath.Join(d, "out.txt")
			writeAtomicSentinel(t, out)
			cfg := Config{
				Inputs:       []string{in},
				Output:       out,
				TempDir:      filepath.Join(d, "shards"),
				Workers:      2,
				DedupWorkers: 2,
				Buckets:      8,
				ChunkBytes:   1 << 20,
				FastPathOff:  !tc.fastPath,
			}
			r, err := Resolve(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r.UseFastPath = tc.fastPath
			m := &Metrics{TotalInputBytes: r.TotalInputs}
			if err := Run(t.Context(), r, m); err != nil {
				t.Fatal(err)
			}
			if m.LinesUnique.Load() != 0 {
				t.Fatalf("LinesUnique = %d, want 0", m.LinesUnique.Load())
			}
			if len(r.OutputPaths) != 0 {
				t.Fatalf("OutputPaths = %v, want empty", r.OutputPaths)
			}
			if got := readAtomicFile(t, out); got != atomicSentinel {
				t.Fatalf("empty run clobbered pre-existing output: got %q", got)
			}
			assertNoTempLeftovers(t, d)
		})
	}
}
