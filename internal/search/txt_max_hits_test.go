package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// H-09: -max-hits-per-chunk was silently ignored in txt mode. The per-file cap
// must truncate like the per-chunk cap does in compressed mode: cap hits
// emitted, OnFileCapped fires exactly once, and the run surfaces the
// truncation instead of reporting complete results.
func TestRunTxtMaxHitsPerFileCapsAndSignals(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "three.txt")
	body := "needle one\nfiller\nneedle two\nfiller\nneedle three\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan Hit, 8)
	var capEvents int
	var cappedEmitted = -1
	err := RunTxt(TxtConfig{
		Ctx:            context.Background(),
		Pattern:        []byte("needle"),
		Workers:        1,
		Files:          []string{p},
		ArchiveOrd:     map[string]int{p: 0},
		Hits:           hitCh,
		MaxHitsPerFile: 1,
		OnFileCapped: func(path string, emitted int) {
			capEvents++
			cappedEmitted = emitted
		},
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}

	var got int
	for range hitCh {
		got++
	}
	if got != 1 {
		t.Fatalf("hits = %d, want 1 (per-file cap)", got)
	}
	if capEvents != 1 {
		t.Fatalf("OnFileCapped fired %d times, want 1", capEvents)
	}
	if cappedEmitted != 1 {
		t.Fatalf("OnFileCapped emitted = %d, want 1", cappedEmitted)
	}
}

// No cap configured: all hits come through and OnFileCapped never fires.
func TestRunTxtMaxHitsPerFileZeroIsUnbounded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "three.txt")
	body := "needle one\nfiller\nneedle two\nfiller\nneedle three\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	hitCh := make(chan Hit, 8)
	var capEvents int
	err := RunTxt(TxtConfig{
		Ctx:        context.Background(),
		Pattern:    []byte("needle"),
		Workers:    1,
		Files:      []string{p},
		ArchiveOrd: map[string]int{p: 0},
		Hits:       hitCh,
		OnFileCapped: func(string, int) {
			capEvents++
		},
	})
	close(hitCh)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for range hitCh {
		got++
	}
	if got != 3 {
		t.Fatalf("hits = %d, want 3 (no cap)", got)
	}
	if capEvents != 0 {
		t.Fatalf("OnFileCapped fired %d times with no cap; want 0", capEvents)
	}
}
