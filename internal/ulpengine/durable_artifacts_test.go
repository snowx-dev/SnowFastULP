package ulpengine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDurableArtifactsIncludesExistingArchiveSidecars(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sfu_run.txt.zst")
	otherArchive := filepath.Join(dir, "sfu_other.txt.zst")
	for _, path := range []string{archive, otherArchive} {
		if err := os.WriteFile(path, []byte("archive"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	idx := sidecarPathForArchive(archive)
	search := searchSidecarPathForArchive(archive)
	for _, path := range []string{idx, search} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{archive, idx, search, otherArchive}
	if got := DurableArtifacts([]string{archive, otherArchive}); !reflect.DeepEqual(got, want) {
		t.Fatalf("DurableArtifacts() = %#v, want %#v", got, want)
	}
}
