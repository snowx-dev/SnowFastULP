package sflog

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// snapSink is a deliberately slow SecretSink that records progress the first
// time it is called — the instant secret scanning starts, after the (fast)
// credential pass has already credited the archive's whole byte weight. Every
// call also samples ScanFraction so a test can assert the bar never regresses.
type snapSink struct {
	prog          *Progress
	mu            sync.Mutex
	seen          bool
	firstByteFrac float64
	firstScanFrac float64
	scanSamples   []float64
}

func (s *snapSink) ScanSecrets(_ context.Context, _ []byte, _ string) int {
	s.mu.Lock()
	if !s.seen {
		s.seen = true
		s.firstByteFrac = s.prog.Fraction()
		s.firstScanFrac = s.prog.ScanFraction()
	}
	s.scanSamples = append(s.scanSamples, s.prog.ScanFraction())
	s.mu.Unlock()
	time.Sleep(2 * time.Millisecond) // make scanning the dominant, observable cost
	return 0
}

// TestSecretScanBarClimbsWhileByteBarComplete is the regression guard for the
// "stalls at 100%" bug. A zip's byte weight is credited by its (fast) credential
// pass, then its allowlisted members are secret-scanned afterwards; previously
// that left the byte bar pinned at 100% with nothing else moving for the whole
// (slow) scan. The scan bar credits a source's whole weight once it is fully
// read and scanned, so this asserts: when scanning starts the Extract bar is
// already at 100% while the Secrets bar has not yet completed, and by the end
// the Secrets bar has reached 100% with every member accounted for.
func TestSecretScanBarClimbsWhileByteBarComplete(t *testing.T) {
	dir := t.TempDir()
	const nSecret = 24
	zp := filepath.Join(dir, "log.zip")
	zf, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	// One credential member drives the byte weight and the password pass.
	cw, _ := zw.Create("Passwords.txt")
	fmt.Fprint(cw, "URL: https://x\nUSER: a\nPASS: b\n")
	for i := 0; i < nSecret; i++ {
		mw, _ := zw.Create(fmt.Sprintf("browser/notes_%d.txt", i))
		fmt.Fprintf(mw, "filler\ntoken ghp_1234567890abcdefghijklmnopqrstuvwx12\nfiller\n")
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}

	prog := NewProgress()
	sink := &snapSink{prog: prog}
	eng := &Engine{
		Workers:      2,
		Passwords:    []string{""},
		SecretSink:   sink,
		SecretMaxLen: defaultSecretMaxLen,
		Progress:     prog,
	}
	var out strings.Builder
	if _, _, err := eng.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !sink.seen {
		t.Fatal("secret sink was never called")
	}
	// The bug: byte bar at 100% but scan work still pending and invisible.
	if sink.firstByteFrac < 0.999 {
		t.Fatalf("byte bar should be complete when scanning starts, got %.3f", sink.firstByteFrac)
	}
	if sink.firstScanFrac >= 1.0 {
		t.Fatalf("scan bar should still be climbing when scanning starts, got %.3f", sink.firstScanFrac)
	}
	// By the end the scan bar caught up and every member was scanned.
	if got := prog.ScanFraction(); got < 0.999 {
		t.Fatalf("scan bar should reach 100%% after the run, got %.3f", got)
	}
	if got := prog.SecretFilesScanned(); got != int64(nSecret) {
		t.Fatalf("SecretFilesScanned = %d, want %d", got, nSecret)
	}
}

// TestSecretScanTotalReachedWithEmptyMembers guards the "bar stalls just shy of
// 100%" gap: a candidate counted in the pre-seeded total but never credited as
// scanned leaves scanned < total forever. Empty/short members are the common
// culprit — scanSecrets used to skip crediting when the read was empty. The zip
// mixes non-empty and empty allowlisted members; all are pre-counted at
// discovery (single-file top-level zip), so the total is fixed up front and
// every member — empty included — must be credited so scanned == total and the
// bar lands exactly on 100%.
func TestSecretScanTotalReachedWithEmptyMembers(t *testing.T) {
	dir := t.TempDir()
	const nFull, nEmpty = 10, 8
	zp := filepath.Join(dir, "log.zip")
	zf, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	cw, _ := zw.Create("Passwords.txt")
	fmt.Fprint(cw, "URL: https://x\nUSER: a\nPASS: b\n")
	for i := 0; i < nFull; i++ {
		mw, _ := zw.Create(fmt.Sprintf("browser/notes_%d.txt", i))
		fmt.Fprint(mw, "filler\ntoken ghp_1234567890abcdefghijklmnopqrstuvwx12\n")
	}
	for i := 0; i < nEmpty; i++ {
		if _, err := zw.Create(fmt.Sprintf("browser/empty_%d.txt", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}

	prog := NewProgress()
	sink := &snapSink{prog: prog}
	eng := &Engine{
		Workers:      3,
		Passwords:    []string{""},
		SecretSink:   sink,
		SecretMaxLen: defaultSecretMaxLen,
		Progress:     prog,
	}
	var out strings.Builder
	if _, _, err := eng.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	const want = nFull + nEmpty
	if got := prog.SecretFilesTotal(); got != int64(want) {
		t.Fatalf("SecretFilesTotal = %d, want %d (all members pre-counted at discovery)", got, want)
	}
	if got := prog.SecretFilesScanned(); got != int64(want) {
		t.Fatalf("SecretFilesScanned = %d, want %d (empty members must still credit scanned)", got, want)
	}
	if got := prog.ScanFraction(); got != 1.0 {
		t.Fatalf("scan bar must land exactly on 100%%, got %.4f", got)
	}
}

// TestSecretScanBarNeverRegresses is the guard for the "bar jumps backwards"
// bug (66% → 57% → 88% → 78%). The old bar was scannedBytes / queuedBytes with
// the denominator credited just before each scan, so it raced ahead of the
// numerator by the (variable) in-flight bytes and the ratio bounced. The fix
// credits each finished source's weight against a fixed total, so the bar can
// only move forward. With many loose files scanned across several workers, the
// sampled ScanFraction must be non-decreasing and finish at 100%.
func TestSecretScanBarNeverRegresses(t *testing.T) {
	dir := t.TempDir()
	const nFiles = 40
	for i := 0; i < nFiles; i++ {
		// Wildly varying sizes are what made the old queued-bytes denominator
		// swing; a monotonic bar must be immune to them.
		size := 1 << (uint(i%8) + 6) // 64B .. 8KiB
		body := strings.Repeat("x", size) + "\nghp_1234567890abcdefghijklmnopqrstuvwx12\n"
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("notes_%d.txt", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prog := NewProgress()
	sink := &snapSink{prog: prog}
	eng := &Engine{
		Workers:      4,
		Passwords:    []string{""},
		SecretSink:   sink,
		SecretMaxLen: defaultSecretMaxLen,
		Progress:     prog,
	}
	var out strings.Builder
	if _, _, err := eng.Run(context.Background(), dir, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(sink.scanSamples) == 0 {
		t.Fatal("no scan samples recorded")
	}
	prev := 0.0
	for i, f := range sink.scanSamples {
		if f < prev-1e-9 {
			t.Fatalf("scan bar regressed at sample %d: %.4f after %.4f", i, f, prev)
		}
		prev = f
	}
	if got := prog.ScanFraction(); got < 0.999 {
		t.Fatalf("scan bar should reach 100%% after the run, got %.3f", got)
	}
	if got := prog.SecretFilesScanned(); got != int64(nFiles) {
		t.Fatalf("SecretFilesScanned = %d, want %d", got, nFiles)
	}
	if got := prog.SecretFilesTotal(); got != int64(nFiles) {
		t.Fatalf("SecretFilesTotal = %d, want %d (all loose candidates seeded up front)", got, nFiles)
	}
}
