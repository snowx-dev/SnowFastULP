package main

import (
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
)

// close must win over a racing ticker draw: once closed, draw never re-enters
// the alt screen, so no AltScreenEnter may follow the final AltScreenLeave.
// The TUI writes to stderrFile (captured at init), so the test swaps it for a
// temp file and reads the emitted control stream back.
func TestSflFrameConcurrentCloseDrawNeverReopens(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	path := f.Name()
	defer os.Remove(path)

	old := stderrFile
	stderrFile = f
	t.Cleanup(func() {
		stderrFile = old
		f.Close()
	})

	frame := stderrFrame{tty: true}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // ticker-equivalent: keep drawing well past the close
		defer wg.Done()
		lines := []string{"extracting", "parsing"}
		for i := 0; i < 300; i++ {
			frame.draw(lines)
			time.Sleep(300 * time.Microsecond)
		}
	}()
	go func() { // the cleanup hook / force-exit path
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		frame.close()
	}()
	wg.Wait()
	f.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	leave := strings.LastIndex(out, termctl.AltScreenLeave)
	if leave < 0 {
		t.Fatalf("frame never left the alt screen:\n%q", out)
	}
	if rest := out[leave+len(termctl.AltScreenLeave):]; strings.Contains(rest, termctl.AltScreenEnter) {
		t.Fatalf("frame re-entered the alt screen after close:\n%q", rest)
	}
	// a draw after close must be a no-op
	wf, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	stderrFile = wf
	before, _ := os.Stat(path)
	frame.draw([]string{"late"})
	after, _ := os.Stat(path)
	stderrFile = old
	wf.Close()
	if after.Size() != before.Size() {
		t.Fatalf("post-close draw wrote bytes")
	}
}

// The -env stats row adds one line to the frame's non-worker overhead; the
// worker-panel budget must include it or a 24-row terminal loses the footer
// under the Compose clamp.
func sflEnvTestProgress() *sflog.Progress {
	p := sflog.NewProgress()
	p.SetWorkers(4)
	return p
}

func TestSflEnvFrameFitsTerminal(t *testing.T) {
	for _, base := range []int{sflPlainFrameOverhead} {
		overhead := base + 1 // + the live Env row
		for h := 16; h <= 80; h++ {
			rows := sflWorkerRows(h, 16, overhead)
			if rows == 0 {
				continue // panel dropped; the box alone fits any usable height
			}
			if frameHeight := overhead + rows; frameHeight > h-1 {
				t.Fatalf("base=%d termHeight=%d: env frame height %d exceeds clamp %d (footer would truncate)",
					base, h, frameHeight, h-1)
			}
		}
	}
	// On the default 24-row terminal the -env frame still shows worker rows
	// while keeping the footer on screen.
	if rows := sflWorkerRows(24, 16, sflPlainFrameOverhead+1); rows < 1 {
		t.Fatalf("24-row -env term dropped the whole worker panel: rows=%d", rows)
	}
}

// TestSflEnvFrame24RowsFooterPresent assembles the plain -env frame exactly
// the way renderProgress does (stats box + Env row + Extract/Deduping bars +
// worker panel budgeted with the +1 Env row) and asserts the footer survives a
// 24-row Compose clamp.
func TestSflEnvFrame24RowsFooterPresent(t *testing.T) {
	const width = 80
	header := headerLine(sflSpinnerStyle.Render(lineSpinnerFrames[0]), sflOkStyle.Render("[sfl] EXTRACTING"), 0, width)
	statRows := renderExtractStatsRows(1, 1, 0, 1, 10, 5, 2, 1<<19, 1<<20, 1e6)
	statRows = append(statRows, renderEnvLiveRow(3, 0))
	plainBox := sflGradientBox(statRows, width, gradStart, gradEnd)
	plainBars := []string{
		sflIndent + sflBarLabel("Extract") + gradientBar(0.5, sflBarBody(width)),
		sflIndent + sflBarLabel("Deduping") + sflPendingBar(sflBarBody(width)),
	}
	panel := sflWorkerPanelBox(sflEnvTestProgress(), width, boxInner(width), 0, sflPlainFrameOverhead+1)
	lines := sflFrameWithBars(header, plainBox, plainBars, panel, width)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "copied") {
		t.Fatalf("-env frame missing the Env stats row:\n%s", joined)
	}
	if len(lines) > 23 {
		t.Fatalf("env frame grew past termHeight-1 (24): %d lines", len(lines))
	}
	tail := strings.Join(lines[len(lines)-3:], "\n")
	if !strings.Contains(tail, "snowx.dev") {
		t.Fatalf("footer dropped on a 24-row -env terminal:\n%s", joined)
	}
}

// fitIngestName must fit any positive budget by terminal cells (wide CJK
// counts 2), never split grapheme clusters, and keep the tail visible.
func TestFitIngestNameCellBudgets(t *testing.T) {
	name := "档案_" + strings.Repeat("文", 20) + "_part4"
	for _, max := range []int{1, 2, 3, 7, 8, 24} {
		got := fitIngestName(name, max)
		if w := tuiframe.VisibleWidth(got); w > max {
			t.Errorf("max=%d: width %d exceeds budget (%q)", max, w, got)
		}
		if strings.Contains(got, "\ufffd") {
			t.Errorf("max=%d: mojibake in %q", max, got)
		}
	}
	// ASCII tail arithmetic: "…part4" needs 6 cells
	ascii := strings.Repeat("x", 40) + "part4"
	for _, max := range []int{6, 12, 24} {
		if got := fitIngestName(ascii, max); !strings.HasSuffix(got, "part4") {
			t.Errorf("max=%d: tail lost: %q", max, got)
		}
	}
	// within budget: unchanged
	if got := fitIngestName("短名.zip", 40); got != "短名.zip" {
		t.Errorf("fitting name altered: %q", got)
	}
}

// clampHead keeps the head (file names) by terminal cells at any budget.
func TestClampHeadCellBudgets(t *testing.T) {
	long := strings.Repeat("文", 30) + "/Passwords.kdbx"
	for _, max := range []int{1, 2, 3, 7, 24} {
		got := clampHead(long, max)
		if w := tuiframe.VisibleWidth(got); w > max {
			t.Errorf("max=%d: width %d exceeds budget (%q)", max, w, got)
		}
		if strings.Contains(got, "\ufffd") {
			t.Errorf("max=%d: mojibake in %q", max, got)
		}
	}
	// head is what clampHead preserves: an ASCII name whose meaningful part
	// leads keeps it, with the ellipsis owning one cell
	ascii := "Passwords.kdbx" + strings.Repeat("x", 40)
	for _, max := range []int{6, 12, 24} {
		got := clampHead(ascii, max)
		if !strings.HasPrefix(got, "Passw") {
			t.Errorf("max=%d: head lost: %q", max, got)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("max=%d: missing ellipsis: %q", max, got)
		}
	}
	if got := clampHead("short.zip", 40); got != "short.zip" {
		t.Errorf("fitting name altered: %q", got)
	}
}

// pokeProgress drives a Progress into a state the engine sets through
// unexported fields (extract phase, byte totals, env counter) so renderProgress
// can be exercised end-to-end without running the engine.
func pokeProgress(t *testing.T, p *sflog.Progress, phase int32, i64s map[string]int64) {
	t.Helper()
	v := reflect.ValueOf(p).Elem()
	poke := func(name string, val any) {
		f := v.FieldByName(name)
		// method call through the pointer value: Value.MethodByName on the
		// element alone does not expose pointer-receiver methods
		ptr := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr()))
		ptr.MethodByName("Store").Call([]reflect.Value{reflect.ValueOf(val)})
	}
	poke("phase", phase)
	for _, name := range []string{"total", "done", "envCopied"} {
		if n, ok := i64s[name]; ok {
			poke(name, n)
		}
	}
}

// TestRenderProgressEnvFrameFits24Rows drives renderProgress (not just its
// helpers) with -env enabled and 16 worker slots, then asserts the composed
// frame fits a 24-row terminal with the footer intact.
func TestRenderProgressEnvFrameFits24Rows(t *testing.T) {
	prog := sflog.NewProgress()
	prog.EnableEnv()
	prog.SetWorkers(16)
	pokeProgress(t, prog, 2 /* phaseExtract */, map[string]int64{
		"total":     1 << 20,
		"done":      1 << 19,
		"envCopied": 3,
	})

	// tests run with stderr NOT a tty, so termHeight() defaults to 24.
	lines := renderProgress(0, prog, 0, 0, 80, true)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "copied") {
		t.Fatalf("-env frame missing the Env stats row:\n%s", joined)
	}
	// Both stage bars render during extraction, mirroring sfu's frame: live
	// Extract plus a queued Deduping track.
	if !strings.Contains(joined, sflBarLabel("Extract")) ||
		!strings.Contains(joined, sflBarLabel("Deduping")+sflPendingBar(sflBarBody(80))) {
		t.Fatalf("-env frame must show both stage bars during extraction:\n%s", joined)
	}
	if len(lines) > 23 {
		t.Fatalf("env frame grew past termHeight-1 (24): %d lines\n%s", len(lines), joined)
	}
	tail := strings.Join(lines[len(lines)-3:], "\n")
	if !strings.Contains(tail, "snowx.dev") {
		t.Fatalf("footer dropped on a 24-row -env terminal:\n%s", joined)
	}
}

// TestRenderProgressSingleBarWithoutOD: a plain -o run has no library
// ingest, so the extraction frame shows only the Extract bar — no queued
// Deduping track that would sit pending forever — and budgets the worker
// panel with the single-bar overhead.
func TestRenderProgressSingleBarWithoutOD(t *testing.T) {
	prog := sflog.NewProgress()
	prog.SetWorkers(2)
	pokeProgress(t, prog, 2 /* phaseExtract */, map[string]int64{
		"total": 1 << 20,
		"done":  1 << 19,
	})
	lines := renderProgress(0, prog, 0, 0, 80, false)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, sflBarLabel("Extract")) {
		t.Fatalf("plain -o frame missing the Extract bar:\n%s", joined)
	}
	if strings.Contains(joined, sflBarLabel("Deduping")) {
		t.Fatalf("plain -o frame must not show the queued Deduping bar:\n%s", joined)
	}
}
