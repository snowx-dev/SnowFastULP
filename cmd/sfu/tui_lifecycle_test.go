package main

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

// wide (CJK) and grapheme-heavy names must truncate by terminal CELLS, not
// bytes or runes, and never leave mojibake in the worker row.
func TestRenderWorkerRowCJKNameFitsCells(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"cjk archive", "sfu_档案_20260514_part4.txt.zst"},
		{"cjk long", "sfu_" + strings.Repeat("书", 30) + "_part4.txt.zst"},
		{"emoji", "sfu_📁" + strings.Repeat("文", 12) + "_part1.txt.zst"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := &ulpengine.WorkerStatus{}
			name := tc.path
			ws.ArchivePath.Store(&name)
			ws.PartIdx.Store(4)
			ws.PartsTotal.Store(16)
			ws.BytesDone.Store(1 << 29)
			ws.BytesTotal.Store(1 << 30)
			// realistic column budget: the OD frame hands each row
			// contentWidth(width); 86-wide terminal -> 78.
			row := renderWorkerRow(0, ws, 78, 5)
			if w := tuiframe.VisibleWidth(row); w > 78 {
				t.Errorf("row width %d exceeds the 78-cell budget:\n%q", w, row)
			}
			// narrow budgets: draw clamps rows by cells, so every row must
			// come out cell-correct without mojibake even at 1-3 columns.
			for _, w := range []int{1, 2, 3, 24} {
				clamped := trimToDisplayWidth(row, w)
				if got := tuiframe.VisibleWidth(clamped); got > w {
					t.Errorf("clamp to %d: width %d exceeds budget (%q)", w, got, clamped)
				}
				if strings.Contains(clamped, "\ufffd") {
					t.Errorf("clamp to %d: mojibake in %q", w, clamped)
				}
			}
			// part annotation must survive truncation at the realistic width
			if !strings.Contains(row, "(4/16)") {
				t.Errorf("part annot dropped: %q", row)
			}
		})
	}
}

// every rendered OD worker row must fit the terminal by cells after the
// frame's own clamp, including budgets of 1, 2, 3, and 24 columns.
func TestWorkerRowClampsToTinyWidths(t *testing.T) {
	ws := &ulpengine.WorkerStatus{}
	name := "sfu_档案_20260514_part4.txt.zst"
	ws.ArchivePath.Store(&name)
	ws.PartIdx.Store(1)
	ws.PartsTotal.Store(1)
	ws.BytesDone.Store(1 << 29)
	ws.BytesTotal.Store(1 << 30)
	row := renderWorkerRow(0, ws, 40, 5)
	for _, w := range []int{1, 2, 3, 24} {
		clamped := trimToDisplayWidth(row, w)
		if got := tuiframe.VisibleWidth(clamped); got > w {
			t.Errorf("clamp to %d: width %d exceeds budget (%q)", w, got, clamped)
		}
	}
}

// regen throughput ETA shares sfs's 99-hour display ceiling: just below it
// renders a duration, beyond it renders an em dash, and stalled throughput
// (rate <= 1 B/s) renders no ETA at all.
func TestRenderODFrameETA(t *testing.T) {
	const tib = 1 << 40
	const rate = float64(1 << 30) // 1 GiB/s
	mk := func(total, done int64) *ulpengine.ODMetrics {
		m := &ulpengine.ODMetrics{}
		m.Phase.Store(int32(ulpengine.ODPhaseRegen))
		m.RegenBytesTotal.Store(total)
		m.RegenBytesRead.Store(done)
		return m
	}
	// near-overflow under the ceiling: 300 TiB at 1 GiB/s = 85.3h
	under := mk(306*tib, 6*tib)
	if secs := float64(300*tib) / rate; secs >= 99*3600 {
		t.Fatalf("fixture drift: expected near-ceiling ETA, got %.0f s", secs)
	}
	out := strings.Join(renderODFrame(under, rate, 100, 0, ""), "\n")
	if !strings.Contains(out, "· ETA 85:20:00") {
		t.Errorf("near-ceiling ETA should render ~85h:\n%s", out)
	}

	// beyond the ceiling: 400 TiB at 1 GiB/s = 113.8h > 99h
	over := mk(406*tib, 6*tib)
	if secs := float64(400*tib) / rate; secs <= 99*3600 {
		t.Fatalf("fixture drift: expected beyond-ceiling ETA, got %.0f s", secs)
	}
	out = strings.Join(renderODFrame(over, rate, 100, 0, ""), "\n")
	if !strings.Contains(out, "· ETA —") {
		t.Errorf("beyond-ceiling ETA should render an em dash:\n%s", out)
	}
	if strings.Contains(out, "ETA ~") {
		t.Errorf("beyond-ceiling ETA leaked a duration:\n%s", out)
	}

	// stalled throughput (rate <= 1 B/s) hides the ETA row entirely
	stalled := mk(306*tib, 6*tib)
	out = strings.Join(renderODFrame(stalled, 0, 100, 0, ""), "\n")
	if strings.Contains(out, "ETA") {
		t.Errorf("stalled throughput rendered an ETA:\n%s", out)
	}
}

// on a 24-row terminal the OD worker rows must shrink to what fits beneath
// the phase-0 box so the Compose clamp never drops the footer (last lines
// carry the frost tagline).
func TestRenderPhase0LinesFooterSurvives24Rows(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(16)
	m.ArchivesNeedRegen.Store(16)
	m.RegenBytesTotal.Store(1 << 40)
	m.Workers = make([]ulpengine.WorkerStatus, 16)
	for i := range m.Workers {
		s := "sfu_档案_part1.txt.zst"
		m.Workers[i].ArchivePath.Store(&s)
		m.Workers[i].PartIdx.Store(1)
		m.Workers[i].PartsTotal.Store(1)
		m.Workers[i].BytesTotal.Store(1 << 30)
		m.Workers[i].BytesDone.Store(int64(i) * (1 << 27))
	}
	r.OdMetrics = m

	// tests run with stderr NOT a tty, so termHeight() defaults to 24.
	lines := renderPhase0Lines(time.Second, &ulpengine.Metrics{}, r, 100, 100, 320e6, 86)
	if len(lines) > 23 {
		t.Fatalf("frame grew past termHeight-1 (24): %d lines", len(lines))
	}
	tail := strings.Join(lines[len(lines)-3:], "\n")
	if !strings.Contains(tail, "https://snowx.dev") {
		t.Fatalf("footer dropped on 24-row terminal:\n%s", strings.Join(lines, "\n"))
	}
	// worker rows must actually be shown (the budget leaves room for some)
	if !strings.Contains(strings.Join(lines, "\n"), "[1]") {
		t.Fatalf("phase-0 frame rendered no worker rows on a 24-row terminal:\n%s",
			strings.Join(lines, "\n"))
	}
}

// when the OD box stacks under an active shard phase on a 24-row terminal and
// the whole OD block cannot fit beneath it, the block is dropped entirely so
// the Compose clamp never eats the footer (last lines carry the frost tagline).
func TestRenderShardLinesODBlockCollapsesOn24Rows(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(16)
	m.ArchivesNeedRegen.Store(16)
	m.RegenBytesTotal.Store(1 << 40)
	m.KeysTotalEstimate.Store(2_400_000)
	m.Workers = make([]ulpengine.WorkerStatus, 16)
	for i := range m.Workers {
		s := "sfu_part1.txt.zst"
		m.Workers[i].ArchivePath.Store(&s)
		m.Workers[i].PartIdx.Store(1)
		m.Workers[i].PartsTotal.Store(1)
		m.Workers[i].BytesTotal.Store(1 << 30)
		m.Workers[i].BytesDone.Store(int64(i) * (1 << 27))
	}
	r.OdMetrics = m

	lines := renderShardLines(time.Now(), time.Second, &ulpengine.Metrics{}, r, 100, 100, 1, 1, 320e6, 86)
	if len(lines) > 23 {
		t.Fatalf("frame grew past termHeight-1 (24): %d lines", len(lines))
	}
	tail := strings.Join(lines[len(lines)-3:], "\n")
	if !strings.Contains(tail, "https://snowx.dev") {
		t.Fatalf("footer dropped on 24-row terminal:\n%s", strings.Join(lines, "\n"))
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "updating library — one-time re-index") {
		t.Fatalf("OD block should have collapsed on the 24-row budget:\n%s", joined)
	}
	if strings.Contains(joined, "in library") {
		t.Fatalf("relocated library total should collapse with the OD block:\n%s", joined)
	}
	for i := 1; i <= maxWorkerRowsRendered; i++ {
		if strings.Contains(joined, "["+strconv.Itoa(i)+"]") {
			t.Fatalf("OD worker rows rendered although the block was collapsed:\n%s", joined)
		}
	}
}

// same collapse rule when the OD box is appended during the later dedup phase.
func TestRenderDedupLinesODBlockCollapsesOn24Rows(t *testing.T) {
	r := &ulpengine.Resolved{
		TotalInputs: 1 << 30, InputFileCount: 1, Workers: 4,
		DedupWorkers: 2, BucketCount: 64,
	}
	m := &ulpengine.ODMetrics{}
	m.Phase.Store(int32(ulpengine.ODPhaseRegen))
	m.ArchivesTotal.Store(16)
	m.ArchivesNeedRegen.Store(16)
	m.RegenBytesTotal.Store(1 << 40)
	m.KeysTotalEstimate.Store(2_400_000)
	m.Workers = make([]ulpengine.WorkerStatus, 16)
	for i := range m.Workers {
		s := "sfu_part1.txt.zst"
		m.Workers[i].ArchivePath.Store(&s)
		m.Workers[i].PartIdx.Store(1)
		m.Workers[i].PartsTotal.Store(1)
		m.Workers[i].BytesTotal.Store(1 << 30)
		m.Workers[i].BytesDone.Store(int64(i) * (1 << 27))
	}
	r.OdMetrics = m
	r.Cfg.DestDedup = true

	met := &ulpengine.Metrics{}
	met.BucketsTotal.Store(64)
	lines := renderDedupLines(time.Now(), time.Second, met, r, 100, 100, 320e6, 86)
	if len(lines) > 23 {
		t.Fatalf("frame grew past termHeight-1 (24): %d lines", len(lines))
	}
	tail := strings.Join(lines[len(lines)-3:], "\n")
	if !strings.Contains(tail, "https://snowx.dev") {
		t.Fatalf("footer dropped on 24-row terminal:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(strings.Join(lines, "\n"), "updating library — one-time re-index") {
		t.Fatalf("OD block should have collapsed on the 24-row budget:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(strings.Join(lines, "\n"), "in library") {
		t.Fatalf("relocated library total should collapse with the OD block:\n%s", strings.Join(lines, "\n"))
	}
}

// close must win over a racing ticker draw: once closed, draw never re-enters
// the alt screen, so no AltScreenEnter may follow the final AltScreenLeave.
func TestTuiFrameConcurrentCloseDrawNeverReopens(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = old
		w.Close()
		r.Close()
	})

	var buf bytes.Buffer
	var drained sync.WaitGroup
	drained.Add(1)
	go func() {
		defer drained.Done()
		_, _ = io.Copy(&buf, r) // drain AND capture, keeps the pipe from filling
	}()

	f := &tuiFrame{tty: true}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // ticker-equivalent: keep drawing well past the close
		defer wg.Done()
		lines := []string{"parsing", "deduping"}
		for i := 0; i < 300; i++ {
			f.draw(lines)
			time.Sleep(300 * time.Microsecond)
		}
	}()
	go func() { // the cleanup hook / force-exit path
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		f.close()
	}()
	wg.Wait()
	w.Close()
	drained.Wait()

	out := buf.String()
	leave := strings.LastIndex(out, termctl.AltScreenLeave)
	if leave < 0 {
		t.Fatalf("frame never left the alt screen:\n%q", out)
	}
	if rest := out[leave+len(termctl.AltScreenLeave):]; strings.Contains(rest, termctl.AltScreenEnter) {
		t.Fatalf("frame re-entered the alt screen after close:\n%q", rest)
	}
	// a draw after close must be a no-op
	f.draw([]string{"late"})
	w2 := captureStderr(t, func() { f.draw([]string{"late"}) })
	if strings.Contains(w2, termctl.AltScreenEnter) {
		t.Fatalf("post-close draw re-entered alt screen: %q", w2)
	}
}

// captureStderr swaps os.Stderr for a temp file, runs fn, and returns what
// was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer os.Remove(f.Name())
	old := os.Stderr
	os.Stderr = f
	defer func() {
		os.Stderr = old
		f.Close()
	}()
	fn()
	f.Close()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}
