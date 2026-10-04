package ulpengine

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/cespare/xxhash/v2"
)

const t06LegacyParserVersion uint64 = 4

func writeLiteralLegacyV2Sidecar(t *testing.T, archivePath string, key uint64) string {
	t.Helper()
	if err := ensureIdxSubdir(filepath.Dir(archivePath)); err != nil {
		t.Fatal(err)
	}
	var header [sidecarBaseHeaderBytes]byte
	copy(header[0:4], sidecarMagic)
	binary.LittleEndian.PutUint16(header[4:6], 2)
	binary.LittleEndian.PutUint16(header[6:8], sidecarHashAlgoXX)
	binary.LittleEndian.PutUint64(header[8:16], 1)
	binary.LittleEndian.PutUint64(header[16:24], t06LegacyParserVersion)
	data := make([]byte, len(header)+SidecarKeyBytes)
	copy(data, header[:])
	binary.LittleEndian.PutUint64(data[len(header):], key)
	path := sidecarPathForArchive(archivePath)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestODRegeneratesLiteralLegacyV2PayloadKeyAndParser(t *testing.T) {
	const oldLine = "example.com:alice:pw | legacy annotation"
	const legacyPreimage = "example.com:alice:pw | legacy annotation"
	host, _, login, password, ok := parseStored(oldLine)
	if !ok || host != "example.com" || login != "alice" || password != "pw | legacy annotation" {
		t.Fatalf("literal legacy payload parsed as (%q,%q,%q), ok=%v", host, login, password, ok)
	}
	legacyKey := xxhash.Sum64String(legacyPreimage)
	currentKey, currentOK := DedupKeyForLine(oldLine, false)
	if !currentOK || currentKey == legacyKey {
		t.Fatalf("fixture key semantics current=%#x legacy=%#x currentOK=%v", currentKey, legacyKey, currentOK)
	}

	libDir := t.TempDir()
	archive := filepath.Join(libDir, "sfu_literal_legacy.txt.zst")
	writeZstdArchive(t, archive, []string{oldLine})
	before, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)
	sidecar := writeLiteralLegacyV2Sidecar(t, archive, legacyKey)
	legacyHeader, err := readSidecarHeader(sidecar)
	if err != errSidecarStale {
		t.Fatalf("literal legacy v2 sidecar error = %v, want stale", err)
	}
	if legacyHeader == nil || legacyHeader.formatVersion != 2 || legacyHeader.parserVersion != t06LegacyParserVersion {
		t.Fatalf("literal legacy header = %+v, want format v2/parser v%d", legacyHeader, t06LegacyParserVersion)
	}

	otherInput := filepath.Join(t.TempDir(), "other.txt")
	writeFileContent(t, otherInput, "other.example.com:bob:otherpw\n")
	_ = runBucketedIngest(t, libDir, otherInput, "trigger_regen")

	hdr, err := readSidecarHeader(sidecar)
	if err != nil {
		t.Fatalf("regenerated sidecar: %v", err)
	}
	if hdr.formatVersion != sidecarFormatV4 || hdr.parserVersion != parserVersion || hdr.keyCount != 1 {
		t.Fatalf("regenerated sidecar header = %+v, want current v4 one-key header", hdr)
	}
	var regenerated []uint64
	if err := streamSidecarKeys(sidecar, func(k uint64) error {
		regenerated = append(regenerated, k)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(regenerated) != 1 || regenerated[0] != currentKey {
		t.Fatalf("regenerated keys = %#v, want current literal-payload key %#x", regenerated, currentKey)
	}
	if regenerated[0] == legacyKey {
		t.Fatalf("regeneration retained obsolete colon-joined key %#x", legacyKey)
	}

	reinput := filepath.Join(t.TempDir(), "reingest.txt")
	writeFileContent(t, reinput, oldLine+"\n")
	metrics := runBucketedIngest(t, libDir, reinput, "literal_legacy_reingest")
	if got := metrics.LinesSkippedByDest.Load(); got != 1 {
		t.Fatalf("re-ingest destination skips = %d, want 1", got)
	}
	if got := metrics.LinesUnique.Load(); got != 0 {
		t.Fatalf("re-ingest unique lines = %d, want 0", got)
	}
	if got := readZstdLines(t, archive); len(got) != 1 || got[0] != oldLine {
		t.Fatalf("legacy payload changed after migration/re-ingest: %q", got)
	}
	after, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHash != sha256.Sum256(after) {
		t.Fatal("legacy archive bytes changed while regenerating sidecar")
	}
}
