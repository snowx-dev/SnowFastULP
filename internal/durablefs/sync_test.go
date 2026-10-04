package durablefs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSyncPathsSyncsFileAndParentMetadata(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "output")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(path, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncPaths([]string{path}); err != nil {
		t.Fatal(err)
	}
}

func TestSyncPathsEmptyIsNoOp(t *testing.T) {
	if err := SyncPaths(nil); err != nil {
		t.Fatal(err)
	}
}

// A supplied file root syncs only the directory whose entry changed — its own
// parent — never the whole ancestor chain up to the volume root, which is
// exactly what fails on Windows when a drive root cannot be opened for write.
func TestCollectTargetsFileRootHasNoAncestorChain(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out", "nested")
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(out, "result.txt")
	if err := os.WriteFile(file, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, dirs, err := collectTargets([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(files, []string{file}) {
		t.Fatalf("files = %v, want [%s]", files, file)
	}
	if !slices.Equal(dirs, []string{out}) {
		t.Fatalf("dirs = %v, want only [%s] (the entry-holding parent)", dirs, out)
	}
	if slices.Contains(dirs, string(filepath.Separator)) {
		t.Fatalf("dirs = %v, must never include the volume root", dirs)
	}
}

// A supplied directory root syncs its own tree plus its immediate parent; the
// ancestors above that hold no changed entries and must stay out of the set.
func TestCollectTargetsDirectoryRootScope(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")
	if err := os.MkdirAll(filepath.Join(out, "part2"), 0o700); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(out, "one.txt")
	two := filepath.Join(out, "part2", "two.txt")
	for path, content := range map[string]string{one: "one", two: "two"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, dirs, err := collectTargets([]string{out})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || !slices.Contains(files, one) || !slices.Contains(files, two) {
		t.Fatalf("files = %v, want both tree files", files)
	}
	if !slices.Contains(dirs, out) || !slices.Contains(dirs, filepath.Join(out, "part2")) {
		t.Fatalf("dirs = %v, want the changed tree directories", dirs)
	}
	// root is out's immediate parent (its directory entry changed); anything
	// above it must not be synced, most of all the volume root.
	if slices.Contains(dirs, string(filepath.Separator)) {
		t.Fatalf("dirs = %v, must never include the volume root", dirs)
	}
	if slices.Contains(dirs, filepath.Dir(root)) {
		t.Fatalf("dirs = %v, must never include ancestors above the immediate parent", dirs)
	}
}

// Deepest-first ordering must be preserved so child directories are flushed
// before their parents.
func TestCollectTargetsOrdersDirsDeepestFirst(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")
	if err := os.MkdirAll(filepath.Join(out, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, dirs, err := collectTargets([]string{out})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(dirs); i++ {
		if len(dirs[i-1]) < len(dirs[i]) {
			t.Fatalf("dirs not deepest-first: %v", dirs)
		}
	}
}
