package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
)

const (
	rateColWidth = 11
	rateEMATau   = 8.0 // sec, ETA rate smoothing window
	minIndexRate = 1024.0
	minChunkRate = 0.05 // chunks/s, ~20s per chunk floor
)

const etaUnknown = time.Duration(-1)

type uiRates struct {
	IndexBPS  float64
	ScanBPS   float64
	ChunkPerS float64
	IndexETA  time.Duration
	SearchETA time.Duration
}

type rateTracker struct {
	prevAt      time.Time
	prevPhase   int32
	prevIndex   int64
	prevScanned int64
	prevChunks  int64
	emaIndexBPS float64
	emaScanBPS  float64
	emaChunkPS  float64
}

func (t *rateTracker) sample(now time.Time, m *search.Metrics) uiRates {
	phase := m.Phase.Load()
	idx := m.IndexBytesDone.Load()
	scanned := m.BytesScanned.Load()
	chunks := m.ChunksDone.Load()

	if phase != t.prevPhase && !t.prevAt.IsZero() {
		t.emaIndexBPS = 0
		t.emaScanBPS = 0
		t.emaChunkPS = 0
	}

	var rates uiRates
	if !t.prevAt.IsZero() && phase == t.prevPhase {
		dt := now.Sub(t.prevAt).Seconds()
		if dt >= 0.05 {
			switch phase {
			case search.PhaseIndex:
				instant := float64(idx-t.prevIndex) / dt
				rates.IndexBPS = instant
				t.emaIndexBPS = emaUpdate(t.emaIndexBPS, instant, dt)
				remaining := m.IndexBytesTotal.Load() - idx
				if remaining < 0 {
					remaining = 0
				}
				rates.IndexETA = etaFromRemaining(float64(remaining), t.emaIndexBPS, minIndexRate)
			case search.PhaseSearch:
				instantScan := float64(scanned-t.prevScanned) / dt
				instantChunk := float64(chunks-t.prevChunks) / dt
				rates.ScanBPS = instantScan
				rates.ChunkPerS = instantChunk
				t.emaScanBPS = emaUpdate(t.emaScanBPS, instantScan, dt)
				t.emaChunkPS = emaUpdate(t.emaChunkPS, instantChunk, dt)
				remainingBytes := m.BytesScannedTotal.Load() - scanned
				if remainingBytes < 0 {
					remainingBytes = 0
				}
				rates.SearchETA = etaFromRemaining(float64(remainingBytes), t.emaScanBPS, minIndexRate)
				if rates.SearchETA < 0 {
					remainingChunks := m.ChunksTotal.Load() - chunks
					if remainingChunks < 0 {
						remainingChunks = 0
					}
					rates.SearchETA = etaFromRemaining(float64(remainingChunks), t.emaChunkPS, minChunkRate)
				}
			}
		}
	}

	t.prevAt = now
	t.prevPhase = phase
	t.prevIndex = idx
	t.prevScanned = scanned
	t.prevChunks = chunks
	return rates
}

func emaUpdate(prev, instant, dtSec float64) float64 {
	if instant <= 0 && prev <= 0 {
		return 0
	}
	if prev <= 0 {
		return instant
	}
	alpha := 1 - math.Exp(-dtSec/rateEMATau)
	return alpha*instant + (1-alpha)*prev
}

func etaFromRemaining(remaining, rate, minRate float64) time.Duration {
	if remaining <= 0 {
		return 0
	}
	if rate < minRate {
		return etaUnknown
	}
	sec := remaining / rate
	const maxSec = 99 * 3600
	if sec > maxSec {
		return etaUnknown
	}
	return time.Duration(sec * float64(time.Second))
}

func formatETA(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	if d < time.Second {
		return "~0s"
	}
	d = d.Round(time.Second)
	if d >= time.Hour {
		h := d / time.Hour
		m := (d % time.Hour) / time.Minute
		if m == 0 {
			return fmt.Sprintf("~%dh", h)
		}
		return fmt.Sprintf("~%dh%02dm", h, m)
	}
	if d >= time.Minute {
		return fmt.Sprintf("~%dm%02ds", d/time.Minute, (d%time.Minute)/time.Second)
	}
	return fmt.Sprintf("~%ds", int(d.Seconds()))
}

func formatRate(bps float64) string {
	if bps <= 0 {
		return "0 B/s"
	}
	if bps < 1024 {
		return fmt.Sprintf("%.0f B/s", bps)
	}
	return formatBytes(int64(bps)) + "/s"
}

func renderThroughputRow(phase int32, rates uiRates) string {
	label := labelStyle.Render("Throughput") + "   "
	switch phase {
	case search.PhaseSearch:
		return label +
			"scan " + byteStyle.Render(padRightPlain(formatRate(rates.ScanBPS), rateColWidth))
	default:
		return label +
			"index " + byteStyle.Render(padRightPlain(formatRate(rates.IndexBPS), rateColWidth))
	}
}

func renderETARow(phase int32, rates uiRates) string {
	label := labelStyle.Render("ETA       ")
	switch phase {
	case search.PhaseSearch:
		return label + timeStyle.Render(formatETA(rates.SearchETA))
	default:
		return label + timeStyle.Render(formatETA(rates.IndexETA))
	}
}

func padRightPlain(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
