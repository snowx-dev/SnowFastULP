package search_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/snowx-dev/SnowFastULP/internal/index"
	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func writeBytesZST(t *testing.T, path string, body []byte) {
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
	if _, err := enc.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
}

// A matched line whose newline falls in a later decode-step must still be
// reported in full. The decoder reads the chunk in DecodeStep-sized reads at
// arbitrary byte boundaries; with many ~54-byte lines and a 4 KiB step, lots of
// lines straddle a seam. Each emitted Hit.Line must equal a complete original
// line — a truncated suffix (newline in the next step) or truncated prefix
// (line start in the previous step) would not be in the expected set.
func TestSearchExtractsCompleteLineAcrossDecodeStepSeam(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seam.zst")

	const n = 4000
	expected := make(map[string]bool, n)
	var body bytes.Buffer
	for i := 0; i < n; i++ {
		// "needle" sits mid-line, so a seam can land before it (prefix cut) or
		// after it (suffix cut); the -END sentinel makes a chopped tail obvious.
		line := fmt.Sprintf("host%05d.example.com:needle:secretpassword-%05d-END", i, i)
		expected[line] = true
		body.WriteString(line)
		body.WriteByte('\n')
	}
	writeBytesZST(t, path, body.Bytes())

	sc, err := index.Build(context.Background(), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan search.Hit, 256)
	var lines []string
	done := make(chan struct{})
	go func() {
		for h := range hitCh {
			lines = append(lines, h.Line)
		}
		close(done)
	}()

	err = search.Run(search.Config{
		Pattern:    []byte("needle"),
		Workers:    1,
		Archives:   []string{path},
		Sidecars:   map[string]*index.Sidecar{path: sc},
		Hits:       hitCh,
		ArchiveOrd: map[string]int{path: 0},
		DecodeStep: 4 << 10, // minimum step → frequent mid-line seams
	})
	close(hitCh)
	<-done
	if err != nil {
		t.Fatal(err)
	}

	if len(lines) != n {
		t.Fatalf("got %d hits, want %d (a seam lost or duplicated a match)", len(lines), n)
	}
	var bad []string
	for _, l := range lines {
		if !expected[l] {
			bad = append(bad, l)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("%d/%d hit lines were truncated at a decode-step seam (not the complete original line); examples:\n  %q\n  %q",
			len(bad), n, bad[0], bad[len(bad)-1])
	}
}

// Plain-text (-txt) counterpart: a line whose match is before the 1 MiB read
// boundary but whose newline is after it must be reported in full. Pre-fix the
// txt path recovered a truncated prefix via on-disk backref but never the
// suffix, so this case came back chopped.
func TestRunTxtExtractsCompleteLineSuffixAcrossReadSeam(t *testing.T) {
	const step = 1 << 20 // mirrors search.defaultDecodeStep
	dir := t.TempDir()
	p := filepath.Join(dir, "suffix.txt")

	// Pad with complete lines to just before the read boundary.
	var body []byte
	filler := []byte("filler-line-aaaaaaaaaaaaaaaaaaa\n") // 32 bytes
	for len(body) < step-200 {
		body = append(body, filler...)
	}
	// Target line: "host:NEEDLE:" lands before the boundary; its long tail and
	// newline land in the next read.
	tail := bytes.Repeat([]byte("x"), 400)
	target := append([]byte("host:NEEDLE:"), tail...)
	body = append(body, target...)
	body = append(body, '\n')
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}

	hits := runTxtCollect(t, p, []byte("NEEDLE"))
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].Line != string(target) {
		t.Fatalf("line truncated at read seam:\n got len %d\nwant len %d", len(hits[0].Line), len(target))
	}
}
