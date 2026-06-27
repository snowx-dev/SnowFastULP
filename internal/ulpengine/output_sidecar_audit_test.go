package ulpengine

import (
	"bufio"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// audit: sidecar keyCount should match archive line count for -od output.
// pre-fix runs indexed via re-parse (parseLoose misses most lines); those
// sidecars stay broken until the archive is re-emitted. set OUTPUT_AUDIT_PATH
// to a .txt.zst from a run after inline sidecar indexing shipped.
func TestAuditOutputArchiveParseRate(t *testing.T) {
	path := os.Getenv("OUTPUT_AUDIT_PATH")
	if path == "" {
		t.Skip("set OUTPUT_AUDIT_PATH to an sfu output .txt.zst")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	br := bufio.NewReaderSize(dec, 4<<20)
	total, okStrict, okLoose := 0, 0, 0
	for {
		line, _, _, rerr := readBoundedLine(br, maxInputLineBytes)
		if rerr != nil {
			if rerr.Error() == "EOF" {
				break
			}
			t.Fatal(rerr)
		}
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		if line == "" {
			continue
		}
		total++
		if _, _, _, _, ok := parse(line); ok {
			okStrict++
		}
		if _, _, _, _, ok := parseLoose(line); ok {
			okLoose++
		}
	}

	hdr, err := readSidecarHeader(sidecarPathForArchive(path))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	keys := hdr.keyCount
	t.Logf("lines=%d strict_ok=%d loose_ok=%d sidecar_keys=%d", total, okStrict, okLoose, keys)

	if total > 0 && keys*10 < uint64(total) {
		t.Errorf("sidecar keys (%d) ≪ archive lines (%d): repeat -od runs cannot dedup prior output", keys, total)
	}
}
