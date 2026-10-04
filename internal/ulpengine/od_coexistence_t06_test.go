package ulpengine

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestODCoexistsWithAnnotatedLegacyRecord(t *testing.T) {
	const oldLine = "example.com:alice:pw | legacy annotation"
	const correctedLine = "example.com:alice:pw"
	oldKey, oldOK := DedupKeyForLine(oldLine, false)
	newKey, newOK := DedupKeyForLine(correctedLine, false)
	if !oldOK || !newOK {
		t.Fatalf("fixture parse old=%v new=%v; both records must have keys", oldOK, newOK)
	}
	if oldKey == newKey {
		t.Fatalf("annotated legacy identity collided with corrected identity: key=%#x", oldKey)
	}

	libDir := t.TempDir()
	oldArchive := filepath.Join(libDir, "sfu_old.txt.zst")
	writeZstdArchive(t, oldArchive, []string{oldLine})
	before, err := os.ReadFile(oldArchive)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)

	input := filepath.Join(t.TempDir(), "corrected.txt")
	writeFileContent(t, input, correctedLine+"\n")
	metrics := runBucketedIngest(t, libDir, input, "corrected")
	if got := metrics.LinesSkippedByDest.Load(); got != 0 {
		t.Fatalf("corrected tuple skipped as destination duplicate: %d", got)
	}
	if got := metrics.LinesUnique.Load(); got != 1 {
		t.Fatalf("corrected tuple unique count = %d, want 1", got)
	}
	storedOld := readZstdLines(t, oldArchive)
	if len(storedOld) != 1 || storedOld[0] != oldLine {
		t.Fatalf("legacy archive bytes changed: stored lines=%q, want literal %q", storedOld, oldLine)
	}
	after, err := os.ReadFile(oldArchive)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != sha256.Sum256(after) {
		t.Fatal("legacy archive payload changed during reindex/ingest")
	}
	storedNew := readZstdLines(t, filepath.Join(libDir, "sfu_corrected.txt.zst"))
	if len(storedNew) != 1 {
		t.Fatalf("new archive contains %d records, want corrected record: %q", len(storedNew), storedNew)
	}
	if storedNew[0] != correctedLine {
		t.Fatalf("new emitted bytes = %q, want exact corrected bytes %q", storedNew[0], correctedLine)
	}
	if _, _, login, password, ok := ParseStoredLine(storedNew[0]); !ok || login != "alice" || password != "pw" {
		t.Fatalf("new stored tuple = (%q,%q), ok=%v; want corrected (alice,pw)", login, password, ok)
	}

	var keys []uint64
	if err := streamSidecarKeys(sidecarPathForArchive(oldArchive), func(k uint64) error {
		keys = append(keys, k)
		return nil
	}); err != nil {
		t.Fatalf("read regenerated legacy sidecar: %v", err)
	}
	if len(keys) != 1 || keys[0] != oldKey {
		t.Fatalf("legacy sidecar keys = %#v, want literal old key %#x", keys, oldKey)
	}
}
