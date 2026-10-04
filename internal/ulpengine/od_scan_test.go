package ulpengine

import (
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// every shape writer can emit + bait shapes that must be rejected
func TestParseArchiveName(t *testing.T) {
	cases := []struct {
		path      string
		wantRunID string
		wantPart  int
	}{
		{"sfu_20260514_abc.txt.zst", "sfu_20260514_abc", 0},
		{"sfu_20260514_abc_part1.txt.zst", "sfu_20260514_abc", 1},
		{"sfu_20260514_abc_part42.txt.zst", "sfu_20260514_abc", 42},
		{"/libs/sfu_xyz.txt.zst", "sfu_xyz", 0},
		{"foreign.zst", "", 0},
		{"sfu_xyz.zst", "", 0}, // missing .txt
		{"sfu_xyz.txt", "", 0}, // missing .zst
		// non-numeric _partXYZ = treat whole thing as runID stem
		{"sfu_xyz_partabc.txt.zst", "sfu_xyz_partabc", 0},
	}
	for _, c := range cases {
		gotID, gotPart := parseArchiveName(c.path)
		if gotID != c.wantRunID || gotPart != c.wantPart {
			t.Errorf("parseArchiveName(%q) = (%q, %d), want (%q, %d)",
				c.path, gotID, gotPart, c.wantRunID, c.wantPart)
		}
	}
}

// 2 single-archive runs + 1 multi-part + foreign noise + self-stamp.
// parts grouped/sorted, foreign filtered, self excluded
func TestDiscoverArchiveRuns(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"sfu_a.txt.zst",
		"sfu_b_part2.txt.zst",
		"sfu_b_part1.txt.zst",
		"sfu_b_part3.txt.zst",
		"sfu_c.txt.zst",
		"sfu_self.txt.zst", // excluded by stamp
		"foreign.zst",
		"random_text.txt",
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := discoverArchiveRuns(dir, "sfu_self")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	gotIDs := make([]string, len(runs))
	for i, r := range runs {
		gotIDs[i] = r.runID
	}
	sort.Strings(gotIDs)
	wantIDs := []string{"sfu_a", "sfu_b", "sfu_c"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Errorf("runIDs = %v, want %v", gotIDs, wantIDs)
	}

	// sfu_b: 3 parts in numeric order
	var bRun archiveRun
	for _, r := range runs {
		if r.runID == "sfu_b" {
			bRun = r
		}
	}
	if len(bRun.parts) != 3 {
		t.Fatalf("sfu_b parts = %d, want 3", len(bRun.parts))
	}
	for i, p := range bRun.parts {
		if p.partNum != i+1 {
			t.Errorf("part %d num = %d, want %d", i, p.partNum, i+1)
		}
	}
	wantSidecar := sidecarPathForArchive(filepath.Join(dir, "sfu_b_part1.txt.zst"))
	if got := bRun.parts[0].sidecarPath; got != wantSidecar {
		t.Errorf("parts[0].sidecarPath = %q, want %q", got, wantSidecar)
	}
}

// TestDiscoverArchiveRunsMetacharDir: os.ReadDir-based discovery must treat
// metacharacters in a user-controlled library dir path as literal data —
// filepath.Glob gave "[vault]" char-class meaning and silently discovered
// nothing.
func TestDiscoverArchiveRunsMetacharDir(t *testing.T) {
	for _, dirName := range []string{"[vault]", "li*brary", "what?ever", "[a-z]"} {
		t.Run(dirName, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, dirName)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			wantPath := filepath.Join(dir, "sfu_20260920_v1.txt.zst")
			if err := os.WriteFile(wantPath, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			// a foreign file and a subdirectory must stay filtered
			if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "sfu_dirname.txt.zst"), 0o755); err != nil {
				t.Fatal(err)
			}

			runs, err := discoverArchiveRuns(dir, "")
			if err != nil {
				t.Fatalf("discover under %q: %v", dirName, err)
			}
			if len(runs) != 1 {
				t.Fatalf("dir %q: runs = %d, want 1 (glob treated metachars as a pattern)", dirName, len(runs))
			}
			if runs[0].runID != "sfu_20260920_v1" || len(runs[0].parts) != 1 {
				t.Fatalf("dir %q: unexpected run %+v", dirName, runs[0])
			}
			if runs[0].parts[0].path != wantPath {
				t.Fatalf("dir %q: part path = %q, want %q", dirName, runs[0].parts[0].path, wantPath)
			}
			if runs[0].parts[0].size != 1 {
				t.Fatalf("dir %q: part size = %d, want 1", dirName, runs[0].parts[0].size)
			}
		})
	}
}

// TestEstimateDestKeyBytesMetacharDir: the bucket sizer must see archives
// under metacharacter-named library dirs too (no Glob on user-controlled
// dirs).
func TestEstimateDestKeyBytesMetacharDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "[vault]")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(dir, "sfu_20260920_est.txt.zst")
	largeSidecarForTest(t, arch, 1<<16) // 64K keys → 512 KiB estimate
	// the archive file itself must exist for discovery to see the entry
	if err := os.WriteFile(arch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := EstimateDestKeyBytes(dir, ""); got != int64(1<<16)*SidecarKeyBytes {
		t.Fatalf("EstimateDestKeyBytes under [vault] dir = %d, want %d", got, int64(1<<16)*SidecarKeyBytes)
	}
}

// a stat failure on a library archive is only tolerable when the archive
// vanished between glob and stat (fs.ErrNotExist): permission, I/O, or any
// other error must fail discovery with the archive path, because silently
// omitting an unreadable member weakens destination dedup.
func TestDiscoverArchiveRunsStatErrors(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "sfu_a.txt.zst")
	b := filepath.Join(dir, "sfu_b.txt.zst")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := statArchive
	t.Cleanup(func() { statArchive = orig })

	t.Run("permission fails closed", func(t *testing.T) {
		statArchive = func(path string) (os.FileInfo, error) {
			if path == a {
				return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
			}
			return os.Stat(path)
		}
		_, err := discoverArchiveRuns(dir, "")
		if err == nil {
			t.Fatal("stat EACCES must fail discovery, got nil error")
		}
		if !strings.Contains(err.Error(), a) {
			t.Errorf("error %v must name the failing archive %s", err, a)
		}
	})

	t.Run("ENOENT skipped as deletion race", func(t *testing.T) {
		statArchive = func(path string) (os.FileInfo, error) {
			if path == b {
				return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
			}
			return os.Stat(path)
		}
		runs, err := discoverArchiveRuns(dir, "")
		if err != nil {
			t.Fatalf("ENOENT must be skipped, got error: %v", err)
		}
		gotIDs := make([]string, 0, len(runs))
		for _, r := range runs {
			gotIDs = append(gotIDs, r.runID)
		}
		if !slices.Equal(gotIDs, []string{"sfu_a"}) {
			t.Errorf("runIDs = %v, want [sfu_a] (sfu_b vanished)", gotIDs)
		}
	})
}

// walks every status: fresh, missing, stale-mtime, stale-version
func TestClassifyPartSidecar(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_x.txt.zst")
	if err := os.WriteFile(archive, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	part := archivePart{
		path:        archive,
		partNum:     0,
		modTime:     time.Now().Add(-time.Hour),
		sidecarPath: sidecarPathForArchive(archive),
	}
	if fi, err := os.Stat(archive); err == nil {
		part.modTime = fi.ModTime()
	}

	if st, _ := classifyPartSidecar(part, true); st != sidecarStatusMissing {
		t.Errorf("status = %v, want missing", st)
	}

	writeSidecarKeysForTest(t, archive, nil)
	if st, _ := classifyPartSidecar(part, true); st != sidecarStatusFresh {
		t.Errorf("status = %v, want fresh", st)
	}

	// legacy v2 sidecar carries no archive-identity binding (H-21): stale
	// (full regen), never "fresh" even with an older mtime
	writeV2Sidecar(t, archive, []uint64{99, 1, 42, 1})
	if st, hdr := classifyPartSidecar(part, true); st != sidecarStatusStale {
		t.Errorf("status (legacy v2) = %v, want stale", st)
	} else if hdr != nil {
		t.Errorf("stale status should carry no usable header, got %+v", hdr)
	}

	// archive newer than sidecar => stale
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(archive, future, future); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(archive); err == nil {
		part.modTime = fi.ModTime()
	}
	if st, _ := classifyPartSidecar(part, true); st != sidecarStatusStale {
		t.Errorf("status (touched) = %v, want stale", st)
	}

	// corrupt parserVersion => stale
	if err := os.Chtimes(archive, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(archive); err == nil {
		part.modTime = fi.ModTime()
	}
	hdr := makeSidecarHeader(0, 4, [32]byte{}, 0, 0, 0)
	for i := 16; i < 24; i++ {
		hdr[i] = 0xff
	}
	if err := os.WriteFile(sidecarPathForArchive(archive), hdr[:], 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := classifyPartSidecar(part, true); st != sidecarStatusStale {
		t.Errorf("status (bad version) = %v, want stale", st)
	}
}

// empty dest is no-op, no dest_keys subdir. -od on empty == -o
func TestRunODScanEmpty(t *testing.T) {
	dir := t.TempDir()
	tempDir := t.TempDir()
	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 0 {
		t.Errorf("ArchivesTotal = %d, want 0", res.ArchivesTotal)
	}
	if len(res.DestSidecarPaths) != 0 {
		t.Errorf("DestSidecarPaths len = %d, want 0", len(res.DestSidecarPaths))
	}
}

// legacy v2 sidecar exists and is current: it carries no archive-identity
// binding, so it regenerates from the archive (one-time decompress, H-21).
func TestRunODScanRegeneratesLegacySidecarOnly(t *testing.T) {
	dir := t.TempDir()
	tempDir := t.TempDir()
	archive := filepath.Join(dir, "sfu_prev.txt.zst")
	lines := []string{"example.com:alice:p1", "foo.org:bob:p2"}
	writeZstdArchive(t, archive, lines)
	keys := []uint64{10, 3, 7, 3, 0xdeadbeef}
	writeV2Sidecar(t, archive, keys)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(archive, past, past); err != nil {
		t.Fatal(err)
	}

	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesRegen != 1 {
		t.Errorf("ArchivesRegen = %d, want 1 (legacy sidecars regenerate)", res.ArchivesRegen)
	}
	if res.ArchivesFresh != 0 {
		t.Errorf("ArchivesFresh = %d, want 0", res.ArchivesFresh)
	}
	if res.ArchivesUpgraded != 0 {
		t.Errorf("ArchivesUpgraded = %d, want 0 (no in-place upgrade anymore)", res.ArchivesUpgraded)
	}
	hdr, err := readSidecarHeader(sidecarPathForArchive(archive))
	if err != nil {
		t.Fatalf("post-regen sidecar: %v", err)
	}
	if hdr.formatVersion != sidecarFormatV4 {
		t.Errorf("sidecar not regenerated as v4: formatVersion=%d", hdr.formatVersion)
	}
	if fi, serr := os.Stat(archive); serr != nil || hdr.archiveSize != fi.Size() {
		t.Errorf("sidecar not bound to archive size: hdr=%d stat=%v err=%v", hdr.archiveSize, fi, serr)
	}
	// regenerated keys come from the ARCHIVE, not from the stale v2 fixture
	got := readAllSidecarKeys(t, sidecarPathForArchive(archive))
	fmtr := newLineFormatter()
	var want []uint64
	for _, ln := range lines {
		host, _, login, password, ok := parseFor(ln, false)
		if !ok {
			t.Fatalf("seed line does not parse: %q", ln)
		}
		want = append(want, fmtr.HashKey(host, login, password))
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("regenerated keys = %v, want %v", got, want)
	}
}

// multi-part run: every part has a legacy v2 sidecar; all regenerate from
// their archives and land bound to the current content.
func TestRunODScanRegeneratesMultiPartLegacySidecars(t *testing.T) {
	dir := t.TempDir()
	runID := "sfu_multi"
	const parts = 4
	credLines := make(map[int][]string, parts)
	for i := 1; i <= parts; i++ {
		name := fmt.Sprintf("%s_part%d.txt.zst", runID, i)
		archive := filepath.Join(dir, name)
		lines := []string{
			fmt.Sprintf("host%d.example.com:u%d:p%d", i, i, i),
			fmt.Sprintf("alt%d.example.com:u%d:p%d", i, i, i),
		}
		credLines[i] = lines
		writeZstdArchive(t, archive, lines)
		keys := []uint64{uint64(i), uint64(i * 1000), uint64(i * 1000), uint64(i << 32)}
		writeV2Sidecar(t, archive, keys)
		p := filepath.Join(dir, fmt.Sprintf("%s_part%d.txt.zst", runID, i))
		past := time.Now().Add(-time.Hour)
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	tempDir := t.TempDir()
	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      tempDir,
		Workers:      2,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 1 || res.FilesTotal != parts {
		t.Errorf("counts = archives %d files %d, want 1 / %d", res.ArchivesTotal, res.FilesTotal, parts)
	}
	if res.ArchivesRegen != 1 {
		t.Errorf("ArchivesRegen = %d, want 1", res.ArchivesRegen)
	}
	fmtr := newLineFormatter()
	for i := 1; i <= parts; i++ {
		path := sidecarPathForArchive(filepath.Join(dir, fmt.Sprintf("%s_part%d.txt.zst", runID, i)))
		hdr, herr := readSidecarHeader(path)
		if herr != nil {
			t.Fatalf("part %d sidecar: %v", i, herr)
		}
		if hdr.formatVersion != sidecarFormatV4 {
			t.Errorf("part %d not v4: formatVersion=%d", i, hdr.formatVersion)
		}
		var want []uint64
		for _, ln := range credLines[i] {
			host, _, login, password, ok := parseFor(ln, false)
			if !ok {
				t.Fatalf("seed line does not parse: %q", ln)
			}
			want = append(want, fmtr.HashKey(host, login, password))
		}
		slices.Sort(want)
		got := readAllSidecarKeys(t, path)
		if !slices.Equal(got, want) {
			t.Errorf("part %d regenerated keys = %v, want %v", i, got, want)
		}
		assertSortedUnique(t, readAllSidecarKeys(t, path))
	}
}

// fresh sorted sidecar exists, no regen. its keys are range-readable per bucket
func TestRunODScanWithFreshSidecar(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_prev.txt.zst")
	if err := os.WriteFile(archive, []byte("dummy archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys := []uint64{1, 2, 100, 200, 0xdeadbeefcafebabe}
	writeSidecarKeysForTest(t, archive, keys)
	// sidecar mtime > archive mtime, age the archive
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(archive, past, past)

	tempDir := t.TempDir()
	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 1 || res.ArchivesFresh != 1 || res.ArchivesRegen != 0 {
		t.Errorf("counts = total=%d fresh=%d regen=%d, want 1/1/0",
			res.ArchivesTotal, res.ArchivesFresh, res.ArchivesRegen)
	}
	if res.TotalKeysLoaded != uint64(len(keys)) {
		t.Errorf("keysLoaded = %d, want %d", res.TotalKeysLoaded, len(keys))
	}

	for _, k := range keys {
		idx := int(bucketIndex(k, 3, true, 4)) // top-bits
		ok, gerr := destBucketHasKey(res, k, idx, 4)
		if gerr != nil {
			t.Fatalf("gather bucket %d: %v", idx, gerr)
		}
		if !ok {
			t.Errorf("bucket %d missing key %d", idx, k)
		}
	}
}

// archive but no sidecar, must stream + hash + write + load.
// "user deleted .idx" / parserVersion bump recovery flow
func TestRunODScanRegenerates(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_prev.txt.zst")
	credLines := []string{
		"example.com:alice:p1",
		"foo.org:bob:p2",
		"baz.net:charlie:p3",
	}
	writeZstdArchive(t, archive, credLines)

	tempDir := t.TempDir()
	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan regen: %v", err)
	}
	if res.ArchivesRegen != 1 {
		t.Errorf("ArchivesRegen = %d, want 1", res.ArchivesRegen)
	}
	if res.TotalKeysLoaded != uint64(len(credLines)) {
		t.Errorf("keysLoaded = %d, want %d", res.TotalKeysLoaded, len(credLines))
	}

	if _, err := os.Stat(sidecarPathForArchive(archive)); err != nil {
		t.Errorf("expected sidecar on disk after regen: %v", err)
	}

	fmtr := newLineFormatter()
	for _, line := range credLines {
		host, _, login, password, ok := parseFor(line, false)
		if !ok {
			t.Fatalf("parse failed for %q (test setup bug)", line)
		}
		h := fmtr.HashKey(host, login, password)
		idx := int(bucketIndex(h, 3, true, 4)) // top-bits
		ok, gerr := destBucketHasKey(res, h, idx, 4)
		if gerr != nil {
			t.Fatalf("gather bucket %d: %v", idx, gerr)
		}
		if !ok {
			t.Errorf("bucket %d missing hash for %q", idx, line)
		}
	}
}

// 1 good + 1 garbage archive. skip the bad one w/ warning, dont fail
func TestRunODScanSkipsCorruptArchive(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "sfu_good.txt.zst")
	bad := filepath.Join(dir, "sfu_bad.txt.zst")
	writeZstdArchive(t, good, []string{"example.com:alice:p1"})
	if err := os.WriteFile(bad, []byte("not a real zst archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	tempDir := t.TempDir()
	res, err := runODScan(context.Background(), odConfig{
		Dest:         dir,
		CurrentRunID: "sfu_self",
		Buckets:      4,
		TempDir:      tempDir,
	}, &ODMetrics{})
	if err != nil {
		t.Fatalf("runODScan: %v", err)
	}
	if res.ArchivesTotal != 2 {
		t.Errorf("total = %d, want 2", res.ArchivesTotal)
	}
	if res.ArchivesSkipped != 1 {
		t.Errorf("skipped = %d, want 1", res.ArchivesSkipped)
	}
	if res.TotalKeysLoaded != 1 {
		t.Errorf("keysLoaded = %d, want 1", res.TotalKeysLoaded)
	}
}

// .zst w/ one cred per line, fakes a past run for od_scan tests
func writeZstdArchive(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range lines {
		if _, err := enc.Write([]byte(ln + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
}

// linear scan, bucket files are tiny in tests
func bucketContainsKey(t *testing.T, path string, want uint64) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bucket: %v", err)
	}
	if len(data)%SidecarKeyBytes != 0 {
		t.Fatalf("bucket %s size %d not multiple of %d", path, len(data), SidecarKeyBytes)
	}
	for i := 0; i < len(data); i += SidecarKeyBytes {
		k := binary.LittleEndian.Uint64(data[i : i+SidecarKeyBytes])
		if k == want {
			return true
		}
	}
	return false
}

// TestArchiveRunIDMatchesParseArchiveName: archiveRunID returns exactly the
// run ID parseArchiveName reports for the run's output archive, for both bare
// and sfu_-prefixed caller stamps (the two forms must name the same run).
func TestArchiveRunIDMatchesParseArchiveName(t *testing.T) {
	cases := []string{
		"20260922_abc",
		"self",
		"sfu_self",   // already ID form: at most one leading sfu_ is trimmed
		"sfu_sfu_x2", // trim only ONE leading sfu_
	}
	for _, stamp := range cases {
		want, _ := parseArchiveName(WithZstExt(DefaultBasename(stamp), true))
		if want == "" {
			t.Fatalf("DefaultBasename(%q) does not parse as an archive", stamp)
		}
		if got := archiveRunID(stamp); got != want {
			t.Errorf("archiveRunID(%q) = %q, want %q (parseArchiveName of %q)",
				stamp, got, want, WithZstExt(DefaultBasename(stamp), true))
		}
	}
	// both caller forms of the same run collapse to one ID
	if archiveRunID("self") != archiveRunID("sfu_self") {
		t.Errorf("bare and prefixed caller stamps must yield the same run ID")
	}
}
