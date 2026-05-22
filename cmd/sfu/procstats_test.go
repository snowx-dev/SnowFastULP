package main

import (
	"testing"
	"time"
)

// burns CPU for at least dur so before/after samples see a delta
func busy(dur time.Duration) {
	deadline := time.Now().Add(dur)
	x := 1
	for time.Now().Before(deadline) {
		for i := 0; i < 1_000_000; i++ {
			x = (x*1103515245 + 12345) & 0x7fffffff
		}
	}
	_ = x
}

func TestProcessCPUTimeIsMonotonic(t *testing.T) {
	first := processCPUTime()
	if first < 0 {
		t.Fatalf("CPU time should not be negative, got %v", first)
	}
	busy(50 * time.Millisecond)
	second := processCPUTime()
	if second < first {
		t.Fatalf("CPU time went backwards: first=%v second=%v", first, second)
	}
}

func TestCurrentRSSBytesIsNonZero(t *testing.T) {
	// test binary has RSS, 0 = real bug on this platform
	if v := currentRSSBytes(); v == 0 {
		t.Fatalf("currentRSSBytes returned 0 on a running process; expected > 0")
	}
}

func TestCpuPercentSamplerReturnsZeroFirstCall(t *testing.T) {
	var prevCPU time.Duration
	var prevTime time.Time
	if pct := cpuPercent(&prevCPU, &prevTime); pct != 0 {
		t.Errorf("first call should return 0 (no baseline), got %.2f", pct)
	}
	if prevTime.IsZero() {
		t.Errorf("first call must seed prevTime")
	}
	busy(50 * time.Millisecond)
	pct := cpuPercent(&prevCPU, &prevTime)
	if pct < 0 {
		t.Errorf("CPU%% must be non-negative, got %.2f", pct)
	}
}
