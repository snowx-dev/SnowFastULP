package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// forces lipgloss truecolor SGR even on non-TTY runner, restored on cleanup
func forceTrueColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// gradient must emit a colour ramp, not a flat fill
func TestGradientBarEmitsMultipleColors(t *testing.T) {
	forceTrueColor(t)
	out := gradientBar(1.0, 80) // fully filled
	parts := strings.Split(out, "\x1b[38;2;")
	if len(parts)-1 < 10 {
		t.Errorf("want >=10 truecolor SGR runs, got %d", len(parts)-1)
	}
	first := strings.SplitN(parts[1], "m", 2)[0]
	last := strings.SplitN(parts[len(parts)-1], "m", 2)[0]
	if first == last {
		t.Errorf("gradient endpoints should differ; both = %q", first)
	}
}

// solidBar (done-state phase-1) must be flat, not gradient
func TestSolidBarEmitsSingleColor(t *testing.T) {
	forceTrueColor(t)
	out := solidBar(0.6, 60, solidGreenFill)
	colors := map[string]bool{}
	for _, p := range strings.Split(out, "\x1b[38;") {
		if p == "" {
			continue
		}
		colors[strings.SplitN(p, "m", 2)[0]] = true
	}
	// fill + empty + muted pct = 3 colours max
	if len(colors) > 3 {
		t.Errorf("solidBar should use <=3 colours, got %d (%v)", len(colors), colors)
	}
}

func TestFooterURLUsesBlueBiasedGradient(t *testing.T) {
	forceTrueColor(t)
	got := renderLiveScreenFooter(80)[2]
	startR, startG, startB := footerGradA.BlendLuv(footerGradB, 0.2).RGB255()
	endR, endG, endB := footerGradA.BlendLuv(footerGradB, 0.7).RGB255()
	wantFirst := fmt.Sprintf("38;2;%d;%d;%d", startR, startG, startB)
	wantLast := fmt.Sprintf("38;2;%d;%d;%d", endR, endG, endB)

	if !strings.Contains(got, wantFirst) {
		t.Fatalf("footer URL missing blue-biased gradient start %s in %q", wantFirst, got)
	}
	if !strings.Contains(got, wantLast) {
		t.Fatalf("footer URL missing gradient end %s in %q", wantLast, got)
	}
	if wantFirst == wantLast {
		t.Fatalf("footer URL gradient endpoints should differ")
	}
}

// visual breathing room: leading blank line, gap between bars
func TestRenderShardLayoutIncludesTopOffsetAndBarGap(t *testing.T) {
	m := &ulpengine.Metrics{}
	r := &ulpengine.Resolved{TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4, DedupWorkers: 2, BucketCount: 64}
	lines := renderShardLines(time.Now(), time.Second, m, r, 100, 100, 1, 1, 0, 80)
	if lines[0] != "" {
		t.Errorf("first line should be a blank top offset, got %q", lines[0])
	}
	var barIdx []int
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "    ") {
			continue
		}
		if strings.Contains(ln, "█") || (strings.Contains(ln, "░") && (strings.Contains(ln, "%") || strings.Contains(ln, "----"))) {
			barIdx = append(barIdx, i)
		}
	}
	if len(barIdx) < 2 {
		t.Fatalf("want 2 progress bar rows, found indices %v", barIdx)
	}
	if barIdx[1] != barIdx[0]+2 || lines[barIdx[0]+1] != "" {
		t.Errorf("expected blank line between bars; barIdx=%v lines between=%q", barIdx, lines[barIdx[0]+1])
	}
}
