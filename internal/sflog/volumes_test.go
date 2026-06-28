package sflog

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSplitPartRole(t *testing.T) {
	cases := []struct {
		path    string
		isPart  bool
		isFirst bool
		key     string // base name of the logical archive when isPart
	}{
		{"/d/big.zip.001", true, true, "big.zip"},
		{"/d/big.zip.002", true, false, "big.zip"},
		{"/d/data.7z.010", true, false, "data.7z"},
		{"/d/data.7z.001", true, true, "data.7z"},
		{"/d/SET.ZIP.001", true, true, "SET.ZIP"}, // case-insensitive ext
		{"/d/plain.zip", false, false, ""},
		{"/d/movie.mp4.001", false, false, ""}, // not a zip/7z logical name
		{"/d/foo.rar.001", false, false, ""},   // rar split not in scope
		{"/d/foo.zip.1", false, false, ""},     // single digit not matched
	}
	for _, tc := range cases {
		isPart, isFirst, key := splitPartRole(tc.path)
		if isPart != tc.isPart || isFirst != tc.isFirst {
			t.Errorf("splitPartRole(%q) = (part=%v,first=%v), want (%v,%v)",
				tc.path, isPart, isFirst, tc.isPart, tc.isFirst)
		}
		if tc.isPart && filepath.Base(key) != tc.key {
			t.Errorf("splitPartRole(%q) key base = %q, want %q", tc.path, filepath.Base(key), tc.key)
		}
	}
}

func TestSplitPartSetContiguity(t *testing.T) {
	// Complete 1..3 set.
	dir := t.TempDir()
	for _, n := range []string{"big.zip.001", "big.zip.002", "big.zip.003"} {
		touch(t, filepath.Join(dir, n))
	}
	parts, complete := splitPartSet(filepath.Join(dir, "big.zip.001"))
	if !complete || len(parts) != 3 {
		t.Fatalf("complete set: parts=%v complete=%v", parts, complete)
	}
	// Ordered ascending.
	want := []string{
		filepath.Join(dir, "big.zip.001"),
		filepath.Join(dir, "big.zip.002"),
		filepath.Join(dir, "big.zip.003"),
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("part[%d] = %q, want %q", i, parts[i], want[i])
		}
	}

	// Gap (missing .002) => incomplete.
	gapDir := t.TempDir()
	touch(t, filepath.Join(gapDir, "g.zip.001"))
	touch(t, filepath.Join(gapDir, "g.zip.003"))
	if parts, complete := splitPartSet(filepath.Join(gapDir, "g.zip.001")); complete {
		t.Fatalf("gap set should be incomplete: parts=%v", parts)
	}

	// A continuation part (not first) is never treated as a set head.
	if _, complete := splitPartSet(filepath.Join(dir, "big.zip.002")); complete {
		t.Fatal("non-first part should not report complete")
	}
}

// fixedWeight/idKey are stub injections so groupArchiveVolumes can be exercised
// without fileWeight/logGroupKey.
func fixedWeight(string) int64      { return 100 }
func idKey(p string) string         { return p }
func sortPaths(s []string) []string { sort.Strings(s); return s }

func TestGroupArchiveVolumesSplit(t *testing.T) {
	dir := t.TempDir()
	// One complete split set + a plain zip alongside.
	for _, n := range []string{"big.zip.001", "big.zip.002", "big.zip.003"} {
		touch(t, filepath.Join(dir, n))
	}
	plain := touch(t, filepath.Join(dir, "loose.zip"))
	archives := sortPaths([]string{
		filepath.Join(dir, "big.zip.001"),
		filepath.Join(dir, "big.zip.002"),
		filepath.Join(dir, "big.zip.003"),
		plain,
	})

	items := groupArchiveVolumes(archives, fixedWeight, idKey)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (one split set + one plain): %+v", len(items), items)
	}
	var split, single *workItem
	for i := range items {
		switch items[i].assembly {
		case assemblySplitParts:
			split = &items[i]
		case assemblySingle:
			single = &items[i]
		}
	}
	if split == nil || single == nil {
		t.Fatalf("missing split/single item: %+v", items)
	}
	if split.path != filepath.Join(dir, "big.zip.001") || len(split.volumes) != 3 {
		t.Fatalf("split item = %+v, want first=.001 with 3 volumes", *split)
	}
	if split.weight != 300 {
		t.Fatalf("split weight = %d, want 300 (sum of 3 parts)", split.weight)
	}
	if single.path != plain {
		t.Fatalf("single item path = %q, want %q", single.path, plain)
	}
}

func TestGroupArchiveVolumesSplitOrphanAndGap(t *testing.T) {
	// Orphan continuation: only .002 present, no .001.
	orphanDir := t.TempDir()
	o2 := touch(t, filepath.Join(orphanDir, "x.zip.002"))
	items := groupArchiveVolumes([]string{o2}, fixedWeight, idKey)
	if len(items) != 1 || !items[0].missingFirstVolume || items[0].assembly != assemblySingle {
		t.Fatalf("orphan: items=%+v, want 1 missing-first skip", items)
	}

	// Incomplete set: .001 + .003, missing .002.
	gapDir := t.TempDir()
	g1 := touch(t, filepath.Join(gapDir, "y.zip.001"))
	g3 := touch(t, filepath.Join(gapDir, "y.zip.003"))
	items = groupArchiveVolumes(sortPaths([]string{g1, g3}), fixedWeight, idKey)
	if len(items) != 1 || !items[0].missingFirstVolume {
		t.Fatalf("gap: items=%+v, want exactly 1 missing-first skip", items)
	}
	if items[0].volumes != nil {
		t.Fatalf("incomplete set must not carry volumes (would read a hole): %+v", items[0])
	}
}

// Regression guard: rar volume grouping must still work through the renamed
// generalized function.
func TestGroupArchiveVolumesRarStillGroups(t *testing.T) {
	dir := t.TempDir()
	p1 := touch(t, filepath.Join(dir, "set.part1.rar"))
	p2 := touch(t, filepath.Join(dir, "set.part2.rar"))
	items := groupArchiveVolumes(sortPaths([]string{p1, p2}), fixedWeight, idKey)
	if len(items) != 1 {
		t.Fatalf("rar volumes: got %d items, want 1: %+v", len(items), items)
	}
	if items[0].assembly != assemblyRarVolumes || len(items[0].volumes) != 2 {
		t.Fatalf("rar item = %+v, want assemblyRarVolumes with 2 volumes", items[0])
	}
}

// TestBuildWorklistDiscoversSplitParts proves the discover.go change: a split
// set under the input root is enqueued as one assemblySplitParts item rather
// than dropped (its .NNN extension is not a recognised archive ext).
func TestBuildWorklistDiscoversSplitParts(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"bundle.zip.001", "bundle.zip.002"} {
		touch(t, filepath.Join(root, n))
	}
	touch(t, filepath.Join(root, "other.zip")) // plain archive still discovered
	if err := os.WriteFile(filepath.Join(root, "Passwords.txt"),
		[]byte("URL: https://e.com\nUSER: u\nPASS: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	items, err := buildWorklist(root, NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	var splitItems, splitParts int
	for _, it := range items {
		if it.assembly == assemblySplitParts {
			splitItems++
		}
		// No raw .NNN part should appear as its own item.
		if isSplitArchivePart(it.path) && it.assembly != assemblySplitParts {
			splitParts++
		}
	}
	if splitItems != 1 {
		t.Fatalf("got %d split items, want exactly 1 (the bundle set): %+v", splitItems, items)
	}
	if splitParts != 0 {
		t.Fatalf("a split part leaked as its own non-split item: %+v", items)
	}
}

// TestRealSplitWorklist is a fast, extraction-free check on real public data:
// point SFL_REAL_SPLIT at a dir holding multi-part archives (e.g. fullz/test4
// with .zip.NNN sets and .partN.rar sets) and confirm discovery+grouping turn
// each split set into one assemblySplitParts item (never dropped, never a stray
// .NNN single item) and the rar sets into assemblyRarVolumes items.
func TestRealSplitWorklist(t *testing.T) {
	dir := os.Getenv("SFL_REAL_SPLIT")
	if dir == "" {
		t.Skip("set SFL_REAL_SPLIT to a dir with .zip.NNN / .partN.rar sets")
	}
	if testing.Short() {
		t.Skip("skipping real-data worklist under -short")
	}
	items, err := buildWorklist(dir, NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	var split, rar, leaked int
	for _, it := range items {
		switch it.assembly {
		case assemblySplitParts:
			split++
		case assemblyRarVolumes:
			rar++
		}
		if isSplitArchivePart(it.path) && it.assembly != assemblySplitParts {
			leaked++
		}
	}
	t.Logf("worklist: %d items, split-sets=%d rar-volume-sets=%d", len(items), split, rar)
	if split == 0 {
		t.Fatalf("expected at least one .zip.NNN/.7z.NNN split set in %s", dir)
	}
	if leaked != 0 {
		t.Fatalf("%d split part(s) leaked as non-split items (silent data loss)", leaked)
	}
}
