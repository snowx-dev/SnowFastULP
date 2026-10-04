package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBOMChunkedCountersNoDoubleCount (L-02, red-first): with a UTF-8 BOM,
// chunk 1's offset bookkeeping started at 0 while its window ends were raw
// offsets, so the chunk read one aligned record past job.end and the next
// chunk re-read that record. The review's fixture — BOM + two 18-byte
// records + 8-byte chunks = 39 source bytes — counted linesRead/accepted=3
// (want 2) while unique stayed 2 because output dedup masked the overlap.
func TestBOMChunkedCountersNoDoubleCount(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")
	body := "\xEF\xBB\xBF" +
		"a.example.com:u:x\n" +
		"b.example.com:v:y\n"
	if len(body) != 39 {
		t.Fatalf("fixture size = %d, want 39 (BOM + 2 records)", len(body))
	}
	if err := os.WriteFile(in, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Config{
		Inputs:      []string{in},
		Output:      filepath.Join(d, "out.txt"),
		TempDir:     filepath.Join(d, "stage"),
		Workers:     1,
		ChunkBytes:  8,
		FastPathOff: true,
		RunStarted:  time.Date(2026, 5, 10, 15, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	m := &Metrics{}
	if err := Run(context.Background(), r, m); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := m.LinesRead.Load(); got != 2 {
		t.Errorf("LinesRead = %d, want 2 (BOM must not let chunks overlap one record)", got)
	}
	if got := m.LinesAccepted.Load(); got != 2 {
		t.Errorf("LinesAccepted = %d, want 2", got)
	}
	if got := m.LinesUnique.Load(); got != 2 {
		t.Errorf("LinesUnique = %d, want 2", got)
	}
	// The BOM bytes are discarded before parsing, so the honest raw-record
	// total is 36 — the same figure a single whole-file chunk reports. The
	// review's observed 39 was the double-count's in-range credit remnant
	// (the shifted window credited 3 phantom bytes), not a target.
	if got := m.BytesRead.Load(); got != 36 {
		t.Errorf("BytesRead = %d, want 36 (matches the whole-file path)", got)
	}
}
