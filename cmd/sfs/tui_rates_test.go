package main

import (
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestFormatETA(t *testing.T) {
	if got := formatETA(etaUnknown); got != "—" {
		t.Fatalf("unknown = %q", got)
	}
	if got := formatETA(0); got != "~0s" {
		t.Fatalf("zero = %q", got)
	}
	if got := formatETA(45 * time.Second); got != "~45s" {
		t.Fatalf("45s = %q", got)
	}
	if got := formatETA(2*time.Minute + 5*time.Second); got != "~2m05s" {
		t.Fatalf("2m5s = %q", got)
	}
	if got := formatETA(2 * time.Hour); got != "~2h" {
		t.Fatalf("2h = %q", got)
	}
}

func TestRateTrackerIndexETA(t *testing.T) {
	var rt rateTracker
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.IndexBytesTotal.Store(100 << 20)
	m.IndexBytesDone.Store(0)

	now := time.Now()
	rt.sample(now, m)
	m.IndexBytesDone.Store(20 << 20)
	rates := rt.sample(now.Add(time.Second), m)
	if rates.IndexETA <= 0 {
		t.Fatalf("expected positive index ETA, got %v", rates.IndexETA)
	}
	wantRough := 4 * time.Second
	if rates.IndexETA < wantRough/2 || rates.IndexETA > wantRough*4 {
		t.Fatalf("index ETA = %v, want ~%v", rates.IndexETA, wantRough)
	}
}

func TestRateTrackerSearchETA(t *testing.T) {
	var rt rateTracker
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	m.BytesScannedTotal.Store(100 << 20)
	m.BytesScanned.Store(0)

	now := time.Now()
	rt.sample(now, m)
	m.BytesScanned.Store(20 << 20)
	rates := rt.sample(now.Add(time.Second), m)
	if rates.SearchETA <= 0 {
		t.Fatalf("expected positive search ETA, got %v", rates.SearchETA)
	}
	if rates.SearchETA < 2*time.Second || rates.SearchETA > 8*time.Second {
		t.Fatalf("search ETA = %v, want ~4s", rates.SearchETA)
	}
}

func TestRateTrackerETAResetsOnPhaseChange(t *testing.T) {
	var rt rateTracker
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.IndexBytesTotal.Store(1 << 30)
	m.IndexBytesDone.Store(100 << 20)

	now := time.Now()
	rt.sample(now, m)
	m.IndexBytesDone.Store(200 << 20)
	rt.sample(now.Add(time.Second), m)

	m.Phase.Store(search.PhaseSearch)
	m.ChunksTotal.Store(50)
	m.ChunksDone.Store(0)
	rates := rt.sample(now.Add(2*time.Second), m)
	if rates.IndexETA >= 0 && rates.IndexETA != 0 {
		t.Fatalf("index ETA after phase change = %v", rates.IndexETA)
	}
}

func TestFormatRate(t *testing.T) {
	if got := formatRate(0); got != "0 B/s" {
		t.Fatalf("formatRate(0) = %q", got)
	}
	if got := formatRate(512); got != "512 B/s" {
		t.Fatalf("formatRate(512) = %q", got)
	}
	if got := formatRate(2 * 1024 * 1024); got != "2.0 MB/s" {
		t.Fatalf("formatRate(2MiB/s) = %q", got)
	}
}

func TestRateTrackerIndexPhase(t *testing.T) {
	var rt rateTracker
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.IndexBytesDone.Store(0)

	now := time.Now()
	rt.sample(now, m)
	m.IndexBytesDone.Store(10 << 20)
	rates := rt.sample(now.Add(500*time.Millisecond), m)
	if rates.IndexBPS <= 0 {
		t.Fatalf("expected positive index rate, got %v", rates.IndexBPS)
	}
}

func TestRateTrackerResetsOnPhaseChange(t *testing.T) {
	var rt rateTracker
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.IndexBytesDone.Store(100 << 20)

	now := time.Now()
	rt.sample(now, m)
	m.IndexBytesDone.Store(200 << 20)
	rt.sample(now.Add(500*time.Millisecond), m)

	m.Phase.Store(search.PhaseSearch)
	m.BytesScanned.Store(50 << 20)
	rates := rt.sample(now.Add(time.Second), m)
	if rates.IndexBPS != 0 {
		t.Fatalf("index rate after phase change = %v, want 0", rates.IndexBPS)
	}
	if rates.ScanBPS != 0 {
		t.Fatalf("scan rate on first search tick = %v, want 0", rates.ScanBPS)
	}

	m.BytesScanned.Store(100 << 20)
	rates = rt.sample(now.Add(1500*time.Millisecond), m)
	if rates.ScanBPS <= 0 {
		t.Fatalf("expected positive scan rate, got %v", rates.ScanBPS)
	}
}

func TestRenderFullShowsThroughput(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseIndex)
	m.ArchivesTotal.Store(4)
	m.IndexBytesTotal.Store(1 << 30)
	m.IndexBytesDone.Store(512 << 20)

	lines := renderFull(time.Now(), time.Now().Add(-time.Second), m, uiRates{IndexBPS: 128 << 20, IndexETA: 2 * time.Minute}, "")
	found := false
	etaFound := false
	for _, ln := range lines {
		if strings.Contains(ln, "Throughput") && strings.Contains(ln, "index") {
			found = true
		}
		if strings.Contains(ln, "ETA") && strings.Contains(ln, "~2m") {
			etaFound = true
		}
	}
	if !found {
		t.Fatalf("throughput row missing:\n%s", strings.Join(lines, "\n"))
	}
	if !etaFound {
		t.Fatalf("ETA row missing:\n%s", strings.Join(lines, "\n"))
	}
}

func TestRenderSearchShowsScannedTotal(t *testing.T) {
	m := &search.Metrics{}
	m.Phase.Store(search.PhaseSearch)
	m.ArchivesTotal.Store(4)
	m.ChunksTotal.Store(100)
	m.ChunksDone.Store(10)
	m.BytesScanned.Store(512 << 20)
	m.BytesScannedTotal.Store(2 << 30)
	m.Hits.Store(3)

	joined := strings.Join(renderFull(time.Now(), time.Now(), m, uiRates{}, ""), "\n")
	if !strings.Contains(joined, "Scanned") {
		t.Fatalf("missing Scanned row:\n%s", joined)
	}
	if !strings.Contains(joined, "512.0 MB") || !strings.Contains(joined, "2.0 GB") {
		t.Fatalf("expected scanned done/total in output:\n%s", joined)
	}
}

func TestRenderSearchThroughputScanOnly(t *testing.T) {
	row := renderThroughputRow(search.PhaseSearch, uiRates{ScanBPS: 93e6})
	if !strings.Contains(row, "scan") {
		t.Fatalf("missing scan in %q", row)
	}
	if strings.Contains(row, "chunks") {
		t.Fatalf("unexpected chunks in %q", row)
	}
}
