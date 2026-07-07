package main

import (
	"testing"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

func TestSearchPercentUsesInFlightScanBytes(t *testing.T) {
	m := &search.Metrics{}
	m.ChunksTotal.Store(313)
	m.ChunksDone.Store(1)
	m.BytesScannedTotal.Store(100)
	m.BytesScanned.Store(64)
	m.BytesChunkDone.Store(3)

	if got := searchPercent(m); got != 0.64 {
		t.Fatalf("searchPercent = %v, want 0.64", got)
	}

	pct, _, _, _, _, _ := phaseVisuals(search.PhaseSearch, m)
	if pct != 0.64 {
		t.Fatalf("phaseVisuals search pct = %v, want 0.64", pct)
	}
}

func TestSearchPercentAdvancesWithoutCompletedChunks(t *testing.T) {
	m := &search.Metrics{}
	m.ChunksTotal.Store(10)
	m.ChunksDone.Store(0)
	m.BytesScannedTotal.Store(100)
	m.BytesScanned.Store(64)
	m.BytesChunkDone.Store(0)

	if got := searchPercent(m); got != 0.64 {
		t.Fatalf("searchPercent = %v, want 0.64", got)
	}
}
