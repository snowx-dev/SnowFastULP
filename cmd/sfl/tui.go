package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/pathdisp"
	"github.com/snowx-dev/SnowFastULP/internal/plural"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"golang.org/x/term"
)

const (
	sflDisplayWidth = 80
	barSuffixWidth  = 8 // " 100.0%"
	// sflLeftPad insets the box on both sides so it sits balanced in the
	// terminal rather than flush-left, matching sfu/sfs.
	sflLeftPad = 4
	sflIndent  = "    " // = sflLeftPad spaces; aligns the title under the box

	// right-aligned frost footer, mirroring sfs
	sflFooterLine1 = "sfl is open-source ❤️"
	sflFooterLine2 = "https://snowx.dev"
)

var (
	sflTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFDDE6")).Bold(true)
	sflOkStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8FA6")).Bold(true)
	sflUniqueStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})
	sflCountStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "33", Dark: "51"})
	sflByteStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "178", Dark: "222"})
	sflMutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6818F"))
	// sflWarnStyle matches sfu's warnStyle (orange) so rejected/skipped numbers
	// and the DRY RUN badge read the same across both CLIs.
	sflWarnStyle         = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	sflLabelStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#E8B6C6")).Bold(true)
	sflSpinnerStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8FA6")).Bold(true)
	sflInterruptBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#E0B040")).
				Padding(0, 2)
	// sflEmptyStyle is the dark empty track for the per-worker frost mini
	// bars only — the main-lane bars (gradient/solid/pending) share one
	// sflMutedStyle track (2026-10-03, sfu empty/muted harmony).
	sflEmptyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#3A242E"))

	// sflDoneFill is the dusty-mauve solid for a completed track: "done,
	// move on" without the live red gradient, and without sfu's sage green
	// (which clashes with sfl's red theme). Parked alternatives (2026-10-04):
	//   A sage green     AdaptiveColor Light:65 Dark:71  (sfu solidGreenFill)
	//   B dusty mauve    #8A5A68  ← current
	//   C warm stone     #7A6A66
	//   D soft champagne #A89070  (tried; felt a bit loud)
	sflDoneFill = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A5A68"))

	// elegant red gradient: muted plum-raspberry -> heart-emoji red (Twemoji ❤️
	// #DD2E44). Heart red is the main color; the start is a light, close plum so
	// it reads as a hint of purple without a long color span. Distinct from
	// sfu/sfs's light indigo->magenta and the amber interrupt accent.
	gradStart, _ = colorful.Hex("#9E3A6E")
	gradEnd, _   = colorful.Hex("#DD2E44")

	// footer taglines, ice blue → icy white (unified with sfu/sfs footers)
	footerGradA, _ = colorful.Hex("#7DD3E8")
	footerGradB, _ = colorful.Hex("#F2F8FC")

	// sflFrostGradA is sfu's frost-blue box/bars start (frostGradA there):
	// the ingest prep box and per-worker regen bars share it so the OD frame
	// reads as the same panel in both CLIs; the icy end is footerGradB.
	sflFrostGradA, _ = colorful.Hex("#3D7EA6")

	// open-source heart in the footer, bright red ❤️
	heartRed = lipgloss.Color("#FF2B2B")
)

// ASCII spinner, 4 frames at 100ms keyed off wall-clock (no animation tick).
var lineSpinnerFrames = []string{"|", "/", "-", "\\"}

// spinnerTick is the shared 100ms animation counter derived from the wall
// clock, so the header and every worker row animate off one monotonic source.
func spinnerTick(now time.Time) int { return int(now.UnixMilli() / 100) }

func spinnerFrame(now time.Time) string {
	return lineSpinnerFrames[mod(spinnerTick(now), len(lineSpinnerFrames))]
}

// workerSpinnerFrames is a soft braille dot cycle for the per-worker rows: a
// subtle, smooth motion that reads as activity without the visual noise of the
// ASCII bar spinner.
var workerSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// workerSpinnerFrame returns the braille frame for tick, phase-shifted by offset
// so each worker row moves slightly out of step — a gentle cascade that makes
// the panel feel alive and reinforces "many things happening at once".
func workerSpinnerFrame(tick, offset int) string {
	return workerSpinnerFrames[mod(tick+offset, len(workerSpinnerFrames))]
}

// mod is a non-negative modulo so a negative wall clock never indexes OOB.
func mod(a, n int) int {
	if n <= 0 {
		return 0
	}
	return ((a % n) + n) % n
}

// headerLine builds the title row that sits ABOVE the box: an indented spinner
// + tag on the left and the elapsed time pushed flush-right (mirrors sfs).
func headerLine(spinnerStyled, tag string, elapsed time.Duration, width int) string {
	left := sflIndent + spinnerStyled + "  " + tag
	right := sflMutedStyle.Render(formatDuration(elapsed))
	pad := (width - sflLeftPad) - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// sflDoneHeader is the final recap frame's header, harmonized with sfu's
// renderDoneLines COMPLETE header: ✓ COMPLETE on the left, elapsed clock
// flush right (muted, like sfl's live headers). sfl's failure signaling
// stays exit-code + terse stderr line, so only the DRY RUN suffix variant
// exists here.
func sflDoneHeader(dryRun bool, elapsed time.Duration, width int) string {
	phase := "COMPLETE"
	if dryRun {
		phase += " · DRY RUN"
	}
	left := sflIndent + sflOkStyle.Render("✓") + "  " + sflOkStyle.Render(phase)
	right := sflMutedStyle.Render(formatDuration(elapsed))
	pad := (width - sflLeftPad) - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// dryRunSuffix appends an amber DRY RUN marker to the live header tag so a
// -odr preview is unmistakable while the run is in flight. Empty on a real run.
func dryRunSuffix(prog *sflog.Progress) string {
	if prog == nil || !prog.DryRun() {
		return ""
	}
	return " " + sflWarnStyle.Render("DRY RUN")
}

// liveHeaderTag appends the DRY RUN warn badge. The old "vs library" /
// "vs N library" narration badge was cut as AI-slop; the library total now
// lives in the ingest Library stat row ("· N in library").
func liveHeaderTag(prog *sflog.Progress, tag string, width int) string {
	return tag + dryRunSuffix(prog)
}

// frostTagline renders text along the ice-blue footer gradient, faint, for the
// footer. The open-source heart (❤ + optional VS16) stays bright red and is
// styled as one grapheme so width libraries and terminals agree; splitting
// SGR across the cluster under-counts by one cell and soft-wraps junk onto
// the next row.
func frostTagline(text string, spanStart, spanEnd float64) string {
	run := []rune(text)
	if len(run) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(run); i++ {
		r := run[i]
		if r == '❤' {
			cluster := string(r)
			if i+1 < len(run) && run[i+1] == '\uFE0F' {
				cluster += string(run[i+1])
				i++
			}
			b.WriteString(lipgloss.NewStyle().Foreground(heartRed).Render(cluster))
			continue
		}
		if r == '\uFE0F' {
			continue
		}
		t := spanStart
		if len(run) > 1 {
			t = spanStart + (spanEnd-spanStart)*float64(i)/float64(len(run)-1)
		}
		c := footerGradA.BlendLuv(footerGradB, t)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Faint(true).Render(string(r)))
	}
	return b.String()
}

// frostTaglineRight right-aligns a frost tagline within width cells so its
// right edge lines up with the box's right edge. Over-budget rows are trimmed
// (same contract as sfu/sfs) so a wide tagline cannot soft-wrap.
func frostTaglineRight(text string, width int, spanStart, spanEnd float64) string {
	styled := frostTagline(text, spanStart, spanEnd)
	if width <= 0 {
		return ""
	}
	vw := tuiVisibleWidth(styled)
	if vw > width {
		return trimToDisplayWidth(styled, width)
	}
	if vw < width {
		return strings.Repeat(" ", width-vw) + styled
	}
	return styled
}

// footerLines is the blank + two right-aligned frost taglines drawn below the
// box, matching sfs's live/summary footer.
func footerLines(width int) []string {
	return summaryFooterLines(width, nil)
}

func summaryFooterLines(width int, notice *selfupdate.Notice) []string {
	right := width - sflLeftPad
	if right < 1 {
		right = 1
	}
	if notice != nil {
		return []string{
			"",
			footerRow(renderUpdateNoticeLine(notice), frostTagline(sflFooterLine1, 0.0, 0.5), right),
			frostTaglineRight(sflFooterLine2, right, 0.2, 0.7),
		}
	}
	return []string{
		"",
		frostTaglineRight(sflFooterLine1, right, 0.0, 0.5),
		frostTaglineRight(sflFooterLine2, right, 0.2, 0.7),
	}
}

func renderUpdateNoticeLine(notice *selfupdate.Notice) string {
	return sflWarnStyle.Render("Update available: v"+notice.Latest) +
		sflMutedStyle.Render(" · run: ") +
		sflTitleStyle.Render(notice.Command)
}

func footerRow(left, right string, width int) string {
	if width < 1 {
		width = 1
	}
	rw := lipgloss.Width(right)
	if rw > width {
		// the decorative right block never gets to widen the row
		right = trimToDisplayWidth(right, width)
		rw = tuiVisibleWidth(right)
	}
	maxLeft := width - rw - 1
	if maxLeft < 0 {
		maxLeft = 0
	}
	if lipgloss.Width(left) > maxLeft {
		left = ""
	}
	lw := lipgloss.Width(left)
	gap := width - lw - rw
	if gap < 1 {
		gap = 1
	}
	if lw+gap+rw > width {
		// tiny width: the right block alone fills the row — drop the left
		// block and right-align rather than overflow the terminal.
		return strings.Repeat(" ", width-rw) + right
	}
	return left + strings.Repeat(" ", gap) + right
}

// stderrFile is the real stderr captured at package init. The TUI renders to
// it directly (not the os.Stderr package var) so that code which temporarily
// reassigns os.Stderr cannot swallow the live TUI's draws or break
// terminal-size reads. Both resolve to fd 2 in normal runs.
var stderrFile = os.Stderr

// stderrIsTTY reports whether the live frame's target (stderr) is a terminal.
// The TUI and the summary both render to stderr, so this — not stdout — is what
// gates colored output and the alt-screen frame (mirrors sfs). A var so tests
// can force TTY mode for the panic-teardown seams.
var stderrIsTTY = func() bool {
	fi, err := stderrFile.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// stdoutIsCharDevice reports whether stdout is a terminal. Tests replace it.
// -json hides the live screen only when the stream and the terminal are
// the same device.
var stdoutIsCharDevice = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// drawPanicHook is a test seam: when non-nil, the monitor goroutine's draw path
// calls it right after a frame render, so tests can inject a panic after the
// alt screen went up and prove the terminal is restored before the crash
// surfaces. Production never sets it.
var drawPanicHook func()

// applyStderrColorProfile downgrades lipgloss to plain ASCII when stderr is not
// a terminal, so a redirected summary/log never accumulates ANSI escapes.
func applyStderrColorProfile() {
	if !stderrIsTTY() {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

func termWidth() int {
	if w := termWidthFull(); w < sflDisplayWidth {
		return w
	}
	return sflDisplayWidth
}

// termWidthFull is the real terminal width, uncapped. The boxes clamp to
// sflDisplayWidth for readability, but the muted issue block above them is free
// to use all the real estate so long provenance lines truncate as late as
// possible. Falls back to sflDisplayWidth when the size can't be read.
func termWidthFull() int {
	w, _, err := term.GetSize(int(stderrFile.Fd()))
	if err != nil || w <= 0 {
		return sflDisplayWidth
	}
	return w
}

// tuiVisibleWidth measures printable terminal cells, skipping ANSI escapes so
// styled lines measure by what the terminal actually shows. Cell measurement
// (wide CJK = 2, grapheme clusters unsplit) lives in the shared tuiframe
// helpers.
func tuiVisibleWidth(s string) int {
	return tuiframe.VisibleWidth(s)
}

// trimToDisplayWidth clamps a (possibly ANSI-styled) line to max printable
// columns, appending an ellipsis. Frame rows are run through this before
// tuiframe.Compose so a line never exceeds the terminal width and soft-wraps
// (which would desync Compose's per-row cursor math and reintroduce ghosting
// on narrow terminals). ANSI preservation and grapheme-safety are owned by
// the shared helper.
func trimToDisplayWidth(s string, max int) string {
	if max < 1 {
		max = 1
	}
	if tuiVisibleWidth(s) <= max {
		return s
	}
	return tuiframe.TruncateRight(s, max-1) + "\033[0m" + "…"
}

// sflPendingBar mirrors sfu's pendingBar: a wordless muted track for a stage
// that has not started yet (no percent suffix, no fake 0.0%).
func sflPendingBar(width int) string {
	if width < barSuffixWidth+2 {
		width = barSuffixWidth + 2
	}
	body := width - barSuffixWidth
	const suffix = "   ----" // 7 chars, matches " 100.0%"
	return sflMutedStyle.Render(strings.Repeat("░", body) + suffix)
}

// gradientBar renders a red progress bar with a right-aligned percent suffix.
func gradientBar(percent float64, width int) string {
	if width < barSuffixWidth+2 {
		width = barSuffixWidth + 2
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	body := width - barSuffixWidth
	fill := int(math.Round(float64(body) * percent))
	if fill > body {
		fill = body
	}
	if fill < 0 {
		fill = 0
	}
	var b strings.Builder
	for i := 0; i < fill; i++ {
		t := 0.0
		if body > 1 {
			t = float64(i) / float64(body-1)
		}
		c := gradStart.BlendLuv(gradEnd, t)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render("█"))
	}
	if rem := body - fill; rem > 0 {
		// Empty track shares sflMutedStyle with sflPendingBar, mirroring
		// sfu's near-identical empty/muted grays: the active and queued
		// bars read as one lane.
		b.WriteString(sflMutedStyle.Render(strings.Repeat("░", rem)))
	}
	b.WriteString(sflMutedStyle.Render(fmt.Sprintf(" %5.1f%%", percent*100)))
	return b.String()
}

// sflMiniGradientBar is the per-worker ingest regen bar: ▆ fill on sfu's
// frost-blue palette (sflFrostGradA → footerGradB, same as sfu's OD worker
// bars) so it does not compete with the plum-red █ ingest bar above, and no
// embedded percent (the caller owns a fixed pct column).
func sflMiniGradientBar(percent float64, width int, start, end colorful.Color) string {
	if width < 1 {
		return ""
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	fill := int(math.Round(float64(width) * percent))
	if fill > width {
		fill = width
	}
	if fill < 0 {
		fill = 0
	}
	var b strings.Builder
	for i := 0; i < fill; i++ {
		t := 0.0
		if width > 1 {
			t = float64(i) / float64(width-1)
		}
		c := start.BlendLuv(end, t)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render("▆"))
	}
	if rem := width - fill; rem > 0 {
		b.WriteString(sflEmptyStyle.Render(strings.Repeat("░", rem)))
	}
	return b.String()
}

// stderrFrame is a fixed alt-screen block redrawn in place each tick. Falls
// back to nothing on a non-TTY so piped runs stay clean. Draw and close are
// serialized so a force-exit (second Ctrl+C / cleanup timeout) can never
// interleave between line writes and spill the frame onto the primary screen.
// close is idempotent, and once closed the frame never re-enters the
// alt screen — a ticker draw racing the close cannot reopen it.
type stderrFrame struct {
	mu     sync.Mutex
	tty    bool
	altOn  bool
	closed bool
}

func (f *stderrFrame) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if !f.tty || !f.altOn {
		return
	}
	fmt.Fprint(stderrFile, termctl.ANSIResetScroll+termctl.ANSIShowCursor+termctl.AltScreenLeave)
	f.altOn = false
}

func (f *stderrFrame) draw(lines []string) {
	if !f.tty || len(lines) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	var b strings.Builder
	if !f.altOn {
		b.WriteString(termctl.AltScreenEnter + termctl.ANSIHideCursor)
		f.altOn = true
	}
	// Clamp every row to the terminal width before composing: a row wider than
	// the terminal soft-wraps, which desyncs Compose's per-row cursor math and
	// reintroduces ghosting on terminals narrower than the box floor.
	w := termWidth()
	clamped := make([]string, len(lines))
	for i, ln := range lines {
		clamped[i] = trimToDisplayWidth(ln, w)
	}
	// Clamp to one row shy of the terminal height so the worker panel can't
	// scroll the buffer on short terminals; Compose erases any rows a taller
	// previous frame (e.g. the extracting panel) left behind.
	b.WriteString(tuiframe.Compose(clamped, termHeight()-1))
	fmt.Fprint(stderrFile, b.String())
}

// monitor samples Progress every ~200ms and redraws the live frame until done.
// It registers the frame's restore hook so a force-exit (second Ctrl-C) or
// fatal error still leaves the alt-screen and shows the cursor.

// sflSysMu guards the live RAM/CPU sample the monitor feeds the ingest
// System row (sfu computes the same stats inline in its draw loop).
var (
	sflSysMu     sync.Mutex
	sflSysRAMMB  float64
	sflSysCPUPct float64
)

func sflSampleSystemStats() {
	sflSysMu.Lock()
	sflSysRAMMB = float64(currentRSSBytes()) / (1024 * 1024)
	sflSysMu.Unlock()
}

// sflCPUSample mirrors sfu's cpuPercent delta tracking.
func sflCPUSample(prevCPU *time.Duration, prevTime *time.Time) float64 {
	now := time.Now()
	procCPU := processCPUTime()
	if prevTime.IsZero() {
		*prevCPU = procCPU
		*prevTime = now
		return 0
	}
	dCPU := procCPU - *prevCPU
	dTime := now.Sub(*prevTime)
	*prevCPU = procCPU
	*prevTime = now
	if dTime <= 0 {
		return 0
	}
	return 100 * float64(dCPU) / float64(dTime)
}

// sflSystemSnapshot returns the latest RAM/CPU pair for the ingest frame.
func sflSystemSnapshot(cpuPct float64) (float64, float64) {
	sflSysMu.Lock()
	defer sflSysMu.Unlock()
	return sflSysRAMMB, cpuPct
}

func monitor(done <-chan struct{}, started time.Time, prog *sflog.Progress, signaled func() bool, wg *sync.WaitGroup, od bool) {
	if wg != nil {
		defer wg.Done()
	}
	frame := stderrFrame{tty: stderrIsTTY()}
	reg.Set(frame.close)
	defer reg.Clear()
	defer frame.close()
	// Registered last so it runs first: a panic in the render/teardown path
	// restores the terminal through the registry before the goroutine dies,
	// then re-panics so the crash still surfaces.
	defer restoreOnPanic()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var prevAt time.Time
	var prevBytes int64
	var byteRate float64 // instant throughput for the Bytes row
	var prevCPU time.Duration
	var prevCPUAt time.Time

	draw := func() {
		now := time.Now()
		if signaled != nil && signaled() {
			frame.draw(renderInterrupt(now.Sub(started), spinnerFrame(now), prog, termWidth(), ulpengine.SnapshotCleanupLog()))
			if drawPanicHook != nil {
				drawPanicHook()
			}
			return
		}
		cur := prog.DoneBytes()
		if !prevAt.IsZero() {
			if dt := now.Sub(prevAt).Seconds(); dt >= 0.05 {
				byteRate = float64(cur-prevBytes) / dt
			}
		}
		prevAt, prevBytes = now, cur
		// live RAM/CPU for the ingest System row (sfu parity)
		sflSampleSystemStats()
		cpuPct := sflCPUSample(&prevCPU, &prevCPUAt)
		sflSysMu.Lock()
		sflSysCPUPct = cpuPct
		sflSysMu.Unlock()
		// the rate follows whichever byte counter the active phase advances
		frame.draw(renderProgress(now.Sub(started), prog, byteRate, spinnerTick(now), termWidth(), od))
		if drawPanicHook != nil {
			drawPanicHook()
		}
	}

	for {
		select {
		case <-done:
			draw()
			return
		case <-ticker.C:
			draw()
		}
	}
}

// boxInner is the text width inside the bordered/padded box after the leftPad
// inset on both sides (2 border cols + 4 padding cols).
func boxInner(width int) int {
	inner := width - 2*sflLeftPad - 6
	if inner < 24 {
		inner = 24
	}
	return inner
}

// insetBox renders body inside style and indents every line by leftPad so the
// frame sits balanced in the terminal instead of flush-left. Used for the solid
// amber interrupt/warn box; the live/summary boxes use sflGradientBox.
func insetBox(style lipgloss.Style, body []string, width int) []string {
	rendered := style.Width(boxInner(width) + 4).Render(strings.Join(body, "\n"))
	pad := strings.Repeat(" ", sflLeftPad)
	lines := strings.Split(rendered, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return lines
}

// sflPadOrTrim pads s with trailing spaces (or trims with an ellipsis) to exactly
// width printable columns, measuring by visible width so ANSI-styled body lines
// stay aligned inside the box.
func sflPadOrTrim(s string, width int) string {
	if width < 0 {
		width = 0
	}
	vw := tuiVisibleWidth(s)
	if vw == width {
		return s
	}
	if vw < width {
		return s + strings.Repeat(" ", width-vw)
	}
	return trimToDisplayWidth(s, width)
}

// sflGradientBox is insetBox's gradient sibling: it frames body in a rounded box
// whose top/bottom borders carry a per-char start->end LUV gradient (the
// verticals use the gradient midpoint), then indents every line by sflLeftPad. It
// reproduces insetBox's geometry exactly (outer = boxInner+6) so swapping a solid
// box for a gradient one never shifts the layout.
func sflGradientBox(body []string, width int, start, end colorful.Color) []string {
	inner := boxInner(width)
	outer := inner + 6
	mid := start.BlendLuv(end, 0.5)
	midStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	border := func(left, right string) string {
		var b strings.Builder
		for i := 0; i < outer; i++ {
			t := 0.0
			if outer > 1 {
				t = float64(i) / float64(outer-1)
			}
			c := start.BlendLuv(end, t)
			ch := "─"
			switch i {
			case 0:
				ch = left
			case outer - 1:
				ch = right
			}
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render(ch))
		}
		return b.String()
	}
	pad := strings.Repeat(" ", sflLeftPad)
	rows := make([]string, 0, len(body)+2)
	rows = append(rows, pad+border("╭", "╮"))
	for _, ln := range body {
		rows = append(rows, pad+midStyle.Render("│")+"  "+sflPadOrTrim(ln, inner)+"  "+midStyle.Render("│"))
	}
	rows = append(rows, pad+border("╰", "╯"))
	return rows
}

// frame is the shared shape for every render: a blank top margin, the header /
// title row, a blank separator, the boxed body, then the footer.
func frame(header string, box []string, width int) []string {
	return frameWithFooter(header, box, width, footerLines(width))
}

func frameWithFooter(header string, box []string, width int, footer []string) []string {
	out := []string{"", header, ""}
	out = append(out, box...)
	return append(out, footer...)
}

// renderProgress draws one live frame. byteRate is the instant throughput of
// whichever byte counter the active phase advances — DoneBytes during
// extract/ingest, the history counters during the checking phase (the
// monitor switches on the same phase the renderer branches on).
//
// sflRenderIngest is the ingest stage's frame, laid out like sfu's -od TUI:
// a spinner + phase-tag header, the OD-style library box (Library title row,
// Library/Bytes/Throughput/System rows) as the primary panel during library
// prep, per-worker rows below the box, and a single bar under the frame.
// During the dedup merge it renders sfu's dedup stat rows (Throughput write,
// Lines unique/rejected, Progress buckets/workers, System, Library matching)
// with a solid stage-1 bar and an active Deduping bar.
//
// sfl runs the ULP shard read concurrently with library prep (the engine
// re-orders PhaseShard/PhasePhase0; see cmd/sfl ingestProgress), so sfu's
// separate PARSING screen cannot exist here; the shard read surfaces as the
// conditional ULP row inside the prep box instead.
func sflRenderIngest(elapsed time.Duration, iv sflog.IngestView, prog *sflog.Progress, tick, width int) []string {
	spinner := lineSpinnerFrames[mod(tick, len(lineSpinnerFrames))]
	inner := boxInner(width)

	deduping := iv.EnginePhase == ulpengine.PhaseDedup || iv.EnginePhase == ulpengine.PhaseDone
	var tag string
	if deduping {
		tag = sflPhaseTagText(2, "DEDUPING")
	} else {
		tag = sflPhaseTagText(1, "LIBRARY PREP")
	}
	// headerLine prepends indent + spinner + two spaces; pass only the tag.
	headerLeft := sflOkStyle.Render(tag)
	// history-skip badge, ingest only (matches sfu's ingestion-header badge):
	// muted, in the badge slot, so the bar totals (pending only) don't look
	// like the run is under-reporting. Nothing when zero.
	if !deduping {
		headerLeft += historySkipBadge(prog)
	}
	headerLeft += dryRunSuffix(prog)
	header := headerLine(sflSpinnerStyle.Render(spinner), headerLeft, elapsed, width)

	// frost footer last so Compose's termHeight-1 clamp never drops it when
	// the prep worker stack is budgeted against these trailing rows (parity
	// with sfu's renderPhase0Lines / extract's sflFrameWithBars).
	footer := footerLines(width)
	var body []string
	if deduping {
		body = sflRenderDedupBlock(iv, inner, width)
	} else {
		// non-worker rows outside the prep block: blank+header+blank (3) +
		// footer. No OD leading-blank strip (unlike sfu phase-0-primary).
		body = sflRenderPrepBlock(iv, tick, width, 3+len(footer))
	}
	out := []string{"", header, ""}
	out = append(out, body...)
	return append(out, footer...)
}

// sflPhaseTagText builds the "[N/2 LABEL]" tag (sfu's renderPhaseTagWithTotal).
func sflPhaseTagText(step int, label string) string {
	return fmt.Sprintf("[%d/2 %s]", step, label)
}

// sflRenderPrepBlock mirrors sfu's renderPhase0Lines + renderODFrame: the
// OD-style library box is the primary panel (Library title row with the
// re-index/upgrade subtitle, Library / Bytes / Throughput / System rows),
// per-worker rows sit BELOW the box, and a single unlabeled bar closes the
// block. Falls back to the overall ingest fraction for the bar when no
// regen denominator exists yet (warm library / discovery), so the bar tracks
// the concurrent ULP read instead of freezing at 0.
//
// outerRows is the caller's non-worker line count already committed outside
// this block (header chrome + frost footer). Worker rows shrink via
// sflIngestWorkerRowCap so Compose's termHeight-1 clamp cannot drop the
// footer; if even the zero-worker block cannot fit, the OD panel is omitted
// (sfu's renderODFrame collapse) and only the aggregate bar remains.
func sflRenderPrepBlock(iv sflog.IngestView, tick, width, outerRows int) []string {
	box := sflIngestODBox(iv, boxInner(width))
	// sfu's OD box is frost-blue → icy white; match it (sflFrostGradA =
	// sfu's frostGradA) so the prep panel aligns with the plum-red
	// extract/recap boxes instead of reading as a white outlier.
	boxLines := sflGradientBox(box, width, sflFrostGradA, footerGradB)

	// single bar below the frame: byte-based during regen, parts-indexed for
	// the upgrade pass, overall ingest fraction as the sfl-only fallback
	pct := iv.Fraction
	if iv.RegenBytesTotal > 0 {
		pct = float64(iv.RegenBytesRead) / float64(iv.RegenBytesTotal)
	} else if iv.PartsRegenTotal > 0 {
		pct = float64(iv.PartsRegenDone) / float64(iv.PartsRegenTotal)
	}
	if pct > 1 {
		pct = 1
	}
	bar := sflIndent + gradientBar(pct, sflBarBody(width))

	// Zero-worker non-worker cost of this block: box + blank + bar.
	// (+1 vs sfu's OD leading blank: sfl has no leading blank here.)
	const blockChrome = 2 // blank before bar + bar
	if termHeight()-1 < outerRows+len(boxLines)+blockChrome {
		return []string{"", bar}
	}

	// per-worker rows below the box, sfu-style (no spinner, no header line).
	// Budget includes the gap row that precedes the worker stack when shown.
	var workerRows []string
	if iv.RegenBytesTotal > 0 && len(iv.Workers) > 0 {
		cap := sflIngestWorkerRowCap(termHeight(), len(iv.Workers), outerRows+len(boxLines)+blockChrome+1)
		if cap > 0 {
			active := iv.Workers
			if cap < len(active) {
				active = active[:cap]
			}
			cols := ingestRegenColumns(boxInner(width), workerIdxMarkerW(len(active)))
			for i, w := range active {
				workerRows = append(workerRows, sflIndent+renderIngestRegenRow(w, cols, tick, i))
			}
		}
	}

	out := make([]string, 0, len(boxLines)+len(workerRows)+3)
	out = append(out, boxLines...)
	if len(workerRows) > 0 {
		out = append(out, "", workerRows[0])
		out = append(out, workerRows[1:]...)
	}
	out = append(out, "", bar)
	return out
}

// sflIngestODBox builds the OD-frame inner rows (sfu's renderODFrame body):
// title row, Library row, optional Bytes/Throughput rows, ULP row, System row.
func sflIngestODBox(iv sflog.IngestView, innerW int) []string {
	// muted panel descriptors, sfu parity: regen keeps the one-time
	// re-index orientation tag; upgrade keeps the warn badge
	headerLine := sflLabelStyle.Render("Library")
	switch ulpengine.ODPhase(iv.ODPhase) {
	case ulpengine.ODPhaseRegen:
		headerLine += " " + sflMutedStyle.Render("· one-time re-index")
	case ulpengine.ODPhaseUpgrade:
		headerLine += " " + sflWarnStyle.Render("DO NOT INTERRUPT")
	}

	libRow := sflLabelStyle.Render("Library     ") + sflCountStyle.Render(fmt.Sprintf("%d %s",
		iv.ArchivesTotal, plural.Noun(int(iv.ArchivesTotal), "archive", "archives")))
	if iv.FilesTotal > 0 && iv.FilesTotal > iv.ArchivesTotal {
		libRow += " " + sflMutedStyle.Render(fmt.Sprintf("across %d %s", iv.FilesTotal, plural.Noun(int(iv.FilesTotal), "file", "files")))
	}
	if iv.PartsRegenTotal > 0 {
		libRow += " " + sflMutedStyle.Render("·") + " " +
			sflCountStyle.Render(fmt.Sprintf("%d / %d parts indexed", iv.PartsRegenDone, iv.PartsRegenTotal))
	} else if iv.PartsUpgradeTotal > 0 {
		libRow += " " + sflMutedStyle.Render("·") + " " +
			sflCountStyle.Render(fmt.Sprintf("%d / %d parts upgraded", iv.PartsRegenDone, iv.PartsUpgradeTotal))
	}
	if iv.ArchivesSkipped > 0 {
		libRow += " " + sflWarnStyle.Render(fmt.Sprintf("%d skipped", iv.ArchivesSkipped))
	}
	if total := iv.LibraryKeys; total > 0 {
		libRow += " " + sflMutedStyle.Render("·") + " " +
			sflCountStyle.Render(formatLibraryCount(total)) + sflMutedStyle.Render(" lines")
	}

	innerLines := []string{headerLine, libRow}

	if iv.RegenBytesTotal > 0 {
		innerLines = append(innerLines, sflStatLabel("Bytes")+
			sflByteStyle.Render(humanBytes(iv.RegenBytesRead))+
			" / "+sflByteStyle.Render(humanBytes(iv.RegenBytesTotal)))
		remaining := iv.RegenBytesTotal - iv.RegenBytesRead
		eta := ""
		if remaining > 0 && iv.RegenBPS > 1 {
			secs := float64(remaining) / iv.RegenBPS
			// same 99-hour display ceiling as sfu/sfs; a raw time.Duration
			// overflows int64 nanoseconds near ~292y
			const maxETASeconds = 99 * 3600
			if secs > maxETASeconds {
				eta = " " + sflMutedStyle.Render("· ETA —")
			} else {
				eta = " " + sflMutedStyle.Render("· ETA "+formatDuration(time.Duration(secs)*time.Second))
			}
		}
		if remaining <= 0 && iv.PartsRegenTotal > 0 && iv.PartsRegenDone < iv.PartsRegenTotal {
			// bytes at EOF but parts still commit their sidecars: a rate
			// would read as a dishonest "0 B/s", so hold a muted placeholder
			innerLines = append(innerLines, sflStatLabel("Throughput  ")+sflMutedStyle.Render("…"))
		} else {
			innerLines = append(innerLines, sflStatLabel("Throughput  ")+
				sflByteStyle.Render(formatRate(iv.RegenBPS))+eta)
		}
	}

	// sfl-only: the concurrent ULP shard read. sfu renders this on its own
	// PARSING screen; sfl's ingest runs shard and library prep together, so
	// the read rides one row inside the prep box while it is in flight.
	if iv.ULPBytes > 0 && iv.BytesRead < iv.ULPBytes {
		innerLines = append(innerLines, sflStatLabel("ULP")+
			sflByteStyle.Render(humanBytes(iv.BytesRead))+
			" / "+sflByteStyle.Render(humanBytes(iv.ULPBytes)))
	}

	if system := sflSystemRow(iv.RAMMB, iv.CPUPct); system != "" {
		innerLines = append(innerLines, system)
	}
	return innerLines
}

// sflRenderDedupBlock mirrors sfu's renderDedupLines minus the OD sub-frame:
// the Lines/Progress/System/Library stat rows, a solid stage-1 bar and an
// active Deduping bar.
func sflRenderDedupBlock(iv sflog.IngestView, inner, width int) []string {
	bd, bt := iv.BucketsDone, iv.BucketsTotal
	pct2 := 0.0
	if bbT := iv.BucketsBytesTotal; bbT > 0 {
		pct2 = float64(iv.BucketsBytesRead) / float64(bbT)
	} else if bt > 0 {
		pct2 = float64(bd) / float64(bt)
	}
	if pct2 > 1 {
		pct2 = 1
	}

	bucketsDigits := sflNumDigits(bt)
	workersDigits := sflNumDigits(int64(iv.DedupWorkers))

	linesInline := sflStatLabel("Lines") +
		sflUniqueStyle.Render(formatInt(int(iv.Unique))) + sflMutedStyle.Render(" unique so far")
	// No "rejected" segment: sfl's dedup input is its own dual-fidelity-checked
	// stable output, so the engine's parse-reject counter is always 0 here —
	// real rejects live in the extraction recap and the issues log (2026-10-04).
	progressInline := sflStatLabel("Progress") +
		"buckets " + sflCountStyle.Render(sflIngestNumber(bd, bucketsDigits)+" / "+formatInt(int(bt))) + "    " +
		"workers " + sflCountStyle.Render(sflIngestNumber(int64(iv.BusyWorkers), workersDigits)+" / "+formatInt(int(iv.DedupWorkers))+" busy")
	systemRow := sflSystemRow(iv.RAMMB, iv.CPUPct)

	// Output row (sfu parity, 2026-10-08): first row of the dedup block so the
	// Lines/unique-so-far row keeps its position across both CLIs.
	outputRow := sflStatLabel("Output") + "write " +
		sflByteStyle.Render(formatRate(iv.WriteBPS))
	innerLines := []string{outputRow}
	innerLines = append(innerLines, sflIngestStatRow("Lines", linesInline, inner)...)
	innerLines = append(innerLines, sflIngestStatRow("Progress", progressInline, inner)...)
	innerLines = append(innerLines, systemRow)

	// live library index scan while each bucket's dest set is loaded
	if total := iv.LibraryKeys; total > 0 {
		done := iv.KeysLoaded
		if done > total {
			done = total
		}
		innerLines = append(innerLines, sflLibraryMatchingRows(done, total, inner)...)
	}

	box := sflGradientBox(innerLines, width, gradStart, gradEnd)

	bars := []string{
		sflIndent + sflBarLabel("Extract") + sflSolidBar(1.0, sflBarBody(width), sflDoneFill),
		sflIndent + sflBarLabel("Deduping") + gradientBar(pct2, sflBarBody(width)),
	}
	out := append([]string{}, box...)
	out = append(out, "", bars[0], "", bars[1])
	return out
}

// sflLibraryMatchingRows mirrors sfu's renderLibraryMatchingRows: one
// "Library      matching · D / T loaded" row when it fits, else label row +
// indented counts row.
func sflLibraryMatchingRows(done, total int64, innerW int) []string {
	const label = "Library      " // 13 cells, matches Progress/System rows
	doneStr := sflCountStyle.Render(formatInt(int(done)))
	totalStr := sflCountStyle.Render(formatInt(int(total)))
	countsPart := doneStr + sflMutedStyle.Render(" / ") + totalStr + sflMutedStyle.Render(" loaded")
	singleLineRest := sflMutedStyle.Render("matching · ") + countsPart
	labelRendered := sflLabelStyle.Render(label)
	totalWidth := lipgloss.Width(label) + lipgloss.Width(singleLineRest)
	if innerW <= 0 || totalWidth <= innerW {
		return []string{labelRendered + singleLineRest}
	}
	indent := strings.Repeat(" ", lipgloss.Width(label))
	return []string{
		labelRendered + sflMutedStyle.Render("matching"),
		indent + countsPart,
	}
}

// sflIngestStatRow mirrors sfu's renderStatRow: inline when it fits innerW,
// else one sublabel per line under the 13-cell label column.
func sflIngestStatRow(label, inline string, innerW int) []string {
	labeled := sflStatLabel(label)
	if innerW <= 0 || lipgloss.Width(inline) <= innerW {
		return []string{labeled + strings.TrimPrefix(inline, labeled)}
	}
	return []string{labeled}
}

// workerIdxMarkerW mirrors sfu's workerIdxMarkerWidth: the "[N] " marker
// column sized for the widest index being rendered.
func workerIdxMarkerW(count int) int {
	if count < 1 {
		return 4
	}
	return 1 + sflNumDigits(int64(count)) + 2
}

// historySkipBadge renders the muted " · N skipped" header badge — empty when
// nothing was skipped or prog is nil. Ingestion headers only (sfl's
// EXTRACTING tag and the ingest frame's LIBRARY PREP tag); DEDUPING/COMPLETE
// stay clean, matching sfu's ingestion-header badge.
func historySkipBadge(prog *sflog.Progress) string {
	if prog == nil {
		return ""
	}
	if n := prog.HistorySkipped(); n > 0 {
		return sflMutedStyle.Render(fmt.Sprintf(" · %d skipped", n))
	}
	return ""
}

func renderProgress(elapsed time.Duration, prog *sflog.Progress, byteRate float64, tick int, width int, od bool) []string {
	inner := boxInner(width)
	spinner := lineSpinnerFrames[mod(tick, len(lineSpinnerFrames))]

	// Ingest carries the same icy frame so the screen never hands off. The
	// stage now mirrors sfu's -od TUI: sfu phase tags ([1/2 LIBRARY PREP] /
	// [2/2 DEDUPING]), an OD-style library box (Library title row,
	// Library/Bytes/Throughput/System rows), per-worker rows below the box,
	// and a bar under the frame. iv.Status stays in the progress model for
	// the JSON side only.
	if prog.Phase() == phaseIngestVal {
		iv, _ := prog.IngestSnapshot()
		iv.RAMMB, iv.CPUPct = sflSystemSnapshot(sflSysCPUPct)
		return sflRenderIngest(elapsed, iv, prog, tick, width)
	}

	scanning := prog.Phase() == phaseDiscoverVal || prog.Total() == 0
	phase := "EXTRACTING"
	if prog.Phase() == phaseDoneVal {
		phase = "COMPLETE"
	} else if scanning {
		phase = "SCANNING"
	}

	tag := sflOkStyle.Render("[sfl] " + phase)
	// history-skip badge on the ingestion header only (EXTRACTING); the
	// ingest frame's LIBRARY PREP tag carries it too, DEDUPING stays clean.
	if phase == "EXTRACTING" {
		tag += historySkipBadge(prog)
	}
	header := headerLine(sflSpinnerStyle.Render(spinner), liveHeaderTag(prog, tag, width), elapsed, width)

	if scanning {
		// During discovery the total weight is unknown, so show a live "found"
		// count instead of a frozen 0% bar.
		body := []string{
			sflMutedStyle.Render("discovering sources… ") +
				sflCountStyle.Render(formatInt(int(prog.Discovered()))) +
				sflMutedStyle.Render(" found"),
		}
		return frame(header, sflGradientBox(body, width, gradStart, gradEnd), width)
	}

	statRows := renderExtractStatsRows(
		prog.Files(), prog.Archives(), prog.Discovered(),
		prog.Logs(), prog.LogsTotal(), prog.Emitted(), prog.Duplicates(),
		prog.DoneBytes(), prog.Total(), byteRate,
	)
	envRows := 0
	if prog.EnvEnabled() {
		// The live Env row adds one box line; it must be part of the worker
		// panel's vertical budget or a 24-row terminal with -env pushes the
		// footer under the Compose clamp.
		envRows = 1
		statRows = append(statRows, renderEnvLiveRow(prog.EnvCopied(), prog.EnvDeduped()))
	}
	// plain frame, sfu-style: stats box, then the stage bars — live Extract,
	// plus the queued Deduping track only on -od runs (plain -o has no
	// library ingest, so that bar would sit pending forever) — then the
	// worker panel in its own box.
	plainBox := sflGradientBox(statRows, width, gradStart, gradEnd)
	plainBars := []string{
		sflIndent + sflBarLabel("Extract") + gradientBar(prog.Fraction(), sflBarBody(width)),
	}
	overhead := sflPlainFrameOverheadSingle
	if od {
		plainBars = append(plainBars, sflIndent+sflBarLabel("Deduping")+sflPendingBar(sflBarBody(width)))
		overhead = sflPlainFrameOverhead
	}
	return sflFrameWithBars(header, plainBox, plainBars, sflWorkerPanelBox(prog, width, inner, tick, overhead+envRows), width)
}

// sflBarLabelW is the fixed label column for bar rows so percent suffixes
// line up under each other (mirrors sfu's progressBarLabel).
const sflBarLabelW = 9 // "Extract  "

func sflBarLabel(name string) string {
	s := sflLabelStyle.Render(name)
	if w := lipgloss.Width(s); w < sflBarLabelW {
		return s + strings.Repeat(" ", sflBarLabelW-w)
	}
	return s
}

// sflBarSpan is the full column width one bar row occupies so it lines up
// border-to-border with the gradient box drawn above it (box outer = inner+6),
// exactly as sfu spans its bars across contentWidth.
func sflBarSpan(width int) int { return boxInner(width) + 6 }

// sflBarBody is the width handed to gradientBar/sflSolidBar once the label
// column is subtracted from the box span.
func sflBarBody(width int) int {
	body := sflBarSpan(width) - sflBarLabelW
	if body < barSuffixWidth+2 {
		body = barSuffixWidth + 2
	}
	return body
}

// sflSolidBar is a single-colour completed bar (mirrors sfu's solidBar): the
// finished track's bar, filled flat rather than with the live gradient.
func sflSolidBar(percent float64, width int, fillStyle lipgloss.Style) string {
	if width < barSuffixWidth+2 {
		width = barSuffixWidth + 2
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	body := width - barSuffixWidth
	fill := int(math.Round(float64(body) * percent))
	if fill > body {
		fill = body
	}
	if fill < 0 {
		fill = 0
	}
	full := fillStyle.Render(strings.Repeat("█", fill))
	// Same muted track as gradientBar/sflPendingBar so the main lane keeps
	// one empty color across phases (sfu's empty/muted harmony).
	empty := sflMutedStyle.Render(strings.Repeat("░", body-fill))
	return full + empty + sflMutedStyle.Render(fmt.Sprintf(" %5.1f%%", percent*100))
}

// sflFrameWithBars lays out the frame the way sfu stacks its progress
// bars: the stats box, then each labeled bar on its own line separated by a
// blank row, then (when non-empty) the worker panel in its own box, then the
// footer. panel is nil for the finalize frame, which has no live workers.
func sflFrameWithBars(header string, box, bars, panel []string, width int) []string {
	out := []string{"", header, ""}
	out = append(out, box...)
	for _, b := range bars {
		out = append(out, "", b)
	}
	if len(panel) > 0 {
		out = append(out, "")
		out = append(out, panel...)
	}
	return append(out, footerLines(width)...)
}

// sflPlainFrameOverhead is the non-worker line count for the two-bar plain
// extract frame: top margin + header + blank (3), the stats box (2 borders + 4
// stat rows = 6), the Extract + Deduping bars with their blank separators
// (4), the worker box's blank separator + 2 borders + header row (4), and the
// footer (3) = 20.
const sflPlainFrameOverhead = 20

// sflPlainFrameOverheadSingle is the single-bar variant (plain -o runs, no
// queued Deduping bar): the same frame minus one bar + separator = 18.
const sflPlainFrameOverheadSingle = 18

// sflWorkerRows is the fixed worker-row count for a stacked frame: as many as
// fit beneath the frame's non-worker overhead, capped at sflMaxWorkerRows and
// the worker count, dropping to 0 on terminals too short for even one row, so
// the footer keeps a constant screen row as the busy-worker count changes.
func sflWorkerRows(termH, totalWorkers, overhead int) int {
	if totalWorkers <= 0 {
		return 0
	}
	rows := termH - 1 - overhead
	if rows > sflMaxWorkerRows {
		rows = sflMaxWorkerRows
	}
	if rows > totalWorkers {
		rows = totalWorkers
	}
	if rows < 0 {
		rows = 0
	}
	return rows
}

// sflPlainWorkerRows sizes the worker box for the single-bar plain frame.
func sflPlainWorkerRows(termH, totalWorkers int) int {
	return sflWorkerRows(termH, totalWorkers, sflPlainFrameOverhead)
}

// sflWorkerPanelBox renders the concurrent-worker panel as its own gradient box
// below the bars (mirroring how sfu stacks its worker frame under the progress
// bars). overhead is the frame's non-worker line count (sflPlainFrameOverhead)
// so the box is sized to fit beneath it. It is drawn at
// a FIXED height — a reserved header row plus a constant number of worker rows
// padded with blanks — so the box, and the footer beneath it, never move as
// workers start and finish. Returns nil only when the terminal is too short to
// fit any worker row.
func sflWorkerPanelBox(prog *sflog.Progress, width, inner, tick, overhead int) []string {
	rows := sflWorkerRows(termHeight(), prog.WorkerCount(), overhead)
	if rows <= 0 {
		return nil
	}
	active := prog.ActiveWorkers(rows)
	idxMarkerW := lipgloss.Width(fmt.Sprintf("[%d]", prog.WorkerCount()))
	// Reserve the header row always (blank when <2 busy) so the box top never
	// toggles height; then the active rows, then blank padding to a constant
	// total of rows+1 body lines.
	body := make([]string, 0, rows+1)
	if len(active) >= 2 {
		body = append(body, sflLabelStyle.Render(fmt.Sprintf("%d workers active", len(active))))
	} else {
		body = append(body, "")
	}
	for _, w := range active {
		body = append(body, renderSflWorkerRow(w, inner, idxMarkerW, tick))
	}
	for len(body) < rows+1 {
		body = append(body, "")
	}
	return sflGradientBox(body, width, gradStart, gradEnd)
}

// renderEnvLiveRow is the live "Envs" counter row: how many env/key files have
// been copied so far under -env, plus the byte-identical duplicates the copier
// skipped once dedup has fired.
func renderEnvLiveRow(copied, deduped int64) string {
	row := recapRow("Envs", sflCountStyle.Render(formatInt(int(copied)))+
		sflMutedStyle.Render(" copied"))
	if deduped > 0 {
		row += sflMutedStyle.Render("  ·  ") +
			sflCountStyle.Render(formatInt(int(deduped))) +
			sflMutedStyle.Render(" dupes")
	}
	return row
}

// renderExtractStatsRows is the labeled live-stats block during extraction.
// One metric group per row mirrors recapCountRows so large counts never compete
// for width inside the box. ETA was removed: archive bursts made remaining/rate
// too inconsistent to be honest, so the row is gone
// along with its per-tick EMA computation.

func renderExtractStatsRows(files, archives, discovered, logs, logsTotal, emitted, dupes, doneBytes, totalBytes int64, byteRate float64) []string {
	// The per-group counter (ex-"Logs", briefly "Input N / M") is gone from
	// the live frame by user request — Sources/Bytes carry the progress.
	rows := []string{
		recapRow("Unique", sflUniqueStyle.Render(formatInt(int(emitted)))+
			sflMutedStyle.Render("  ·  ")+sflCountStyle.Render(formatInt(int(dupes)))+
			sflMutedStyle.Render(" dupes")),
	}
	var sourcesVal string
	// F4 (#7) live anchor: Files count everything processed so far — nested
	// archive members included — so a run seeded with 2 archives can climb
	// to "677 files" with no trace of what was passed. Once processing
	// outpaces discovery, anchor the row with the discovered top-level count
	// (the same number the SCANNING frame reported) so the growing numbers
	// stay traceable to real input. The archive/"logs" counter is omitted
	// from this row by design.
	if discovered > 0 && files+archives > discovered {
		sourcesVal = sflCountStyle.Render(formatInt(int(discovered))) +
			sflMutedStyle.Render(" found  ·  ")
	}
	sourcesVal += sflCountStyle.Render(formatInt(int(files))) +
		sflMutedStyle.Render(" "+plural.Noun(int(files), "file", "files"))
	rows = append(rows, recapRow("Sources", sourcesVal))
	rows = append(rows,
		recapRow("Bytes", sflByteStyle.Render(formatBytes(doneBytes))+
			sflMutedStyle.Render(" / "+formatBytes(totalBytes)+"  ·  ")+
			sflByteStyle.Render(formatBytes(int64(byteRate))+"/s")))
	return rows
}

const (
	sflIngestReservedRows = 18
	sflIngestMaxRegenRows = 8
	// per-worker ingest regen row: bars share one column so mixed-length
	// archive names don't stair-step. 22-cell ▆ bar matches sfu's OD workers
	// (distinct from the main █ ingest bar above them).
	sflIngestWorkerBarW    = 22
	sflIngestWorkerPctW    = 4 // " 50%" / "  ?%"
	sflIngestWorkerByteW   = 8 // "999.9 GB" / sfl's "1023.9GB"
	sflIngestWorkerBarMin  = 4
	sflIngestWorkerNameMin = 12
)

// sflIngestWorkerRowCap mirrors sfu's workerRowCap: never more than the
// total worker slots, never more than maxWorkerRowsRendered, and never so
// many that the frame's own rows push the footer past the terminal clamp.
const maxWorkerRowsRendered = 8

func sflIngestWorkerRowCap(termH, totalWorkers, nonWorkerRows int) int {
	if totalWorkers <= 0 {
		return 0
	}
	available := termH - 1 - nonWorkerRows
	if available > totalWorkers {
		available = totalWorkers
	}
	if available > maxWorkerRowsRendered {
		available = maxWorkerRowsRendered
	}
	if available < 0 {
		available = 0
	}
	return available
}

// ingestRegenCols is the shared column budget for every ingest worker row.
// Computed once per panel so name length cannot move the bar, and always
// <= inner so sflPadOrTrim cannot chop the bar off the right.
type ingestRegenCols struct {
	idxW, leftW, barW int
	showBytes         bool
}

func (c ingestRegenCols) width() int {
	n := c.idxW + 3 + c.leftW + 2 + c.barW + 1 + sflIngestWorkerPctW
	if c.showBytes {
		n += 2 + sflIngestWorkerByteW + 3 + sflIngestWorkerByteW
	}
	return n
}

func ingestRegenColumns(inner, idxW int) ingestRegenCols {
	if idxW < 1 {
		idxW = 1
	}
	c := ingestRegenCols{
		idxW: idxW, leftW: sflIngestWorkerNameMin,
		barW: sflIngestWorkerBarW, showBytes: true,
	}
	for c.width() > inner {
		switch {
		case c.showBytes && c.barW > sflIngestWorkerBarMin:
			c.barW--
		case c.showBytes:
			c.showBytes = false
			c.barW = sflIngestWorkerBarW
		case c.barW > sflIngestWorkerBarMin:
			c.barW--
		case c.leftW > 1:
			c.leftW--
		case c.barW > 1:
			c.barW--
		default:
			return c
		}
	}
	if extra := inner - c.width(); extra > 0 {
		c.leftW += extra
	}
	return c
}

// renderIngestRegenRow is one per-worker line for the ingest OD block,
// column-aligned across rows exactly like sfu's renderWorkerRow:
//
//	"[1] xyz_part04   (4/16)  ▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆  36%   1.0 GB / 2.1 GB"
//
// No per-row spinner (sfu parity). During the sidecar commit window the
// frozen byte totals swap for a "committing…" label so a finished stream
// never reads as a finished part.
func renderIngestRegenRow(w sflog.IngestWorker, cols ingestRegenCols, tick, idx int) string {
	displayName, partAnnot := fitIngestLeft(compactIngestArchiveName(w.Archive), partAnnotOf(w), cols.leftW)
	leftPlain := displayName
	if partAnnot != "" {
		leftPlain += " " + partAnnot
	}

	var pct float64
	if w.BytesTotal > 0 {
		pct = float64(w.BytesDone) / float64(w.BytesTotal)
		if pct > 1 {
			pct = 1
		}
	}
	pctText := "  ?%"
	if w.BytesTotal > 0 {
		pctText = fmt.Sprintf("%3d%%", int(pct*100))
	}

	var styledLeft strings.Builder
	styledLeft.WriteString(sflCountStyle.Render(displayName))
	if partAnnot != "" {
		styledLeft.WriteString(" ")
		styledLeft.WriteString(sflMutedStyle.Render(partAnnot))
	}
	if pad := cols.leftW - lipgloss.Width(leftPlain); pad > 0 {
		styledLeft.WriteString(strings.Repeat(" ", pad))
	}

	marker := fmt.Sprintf("[%d]", idx+1)
	if pad := cols.idxW - lipgloss.Width(marker); pad > 0 {
		marker += strings.Repeat(" ", pad)
	}

	// bytes column: padded values so "/" and totals stack vertically;
	// the commit state swaps the totals for the state label
	var bytesText string
	switch {
	case w.Committing:
		bytesText = "committing…"
	case w.BytesTotal > 0:
		bytesText = padLeft(humanBytes(w.BytesDone), sflIngestWorkerByteW) + " / " +
			padLeft(humanBytes(w.BytesTotal), sflIngestWorkerByteW)
	}
	if !w.Committing && w.BytesTotal <= 0 {
		bytesText = strings.Repeat(" ", sflIngestWorkerByteW+3+sflIngestWorkerByteW)
	}

	line := sflMutedStyle.Render(marker) + "  " +
		styledLeft.String() + "  " +
		sflMiniGradientBar(pct, cols.barW, sflFrostGradA, footerGradB) + " " +
		sflMutedStyle.Render(pctText)
	if bytesText != "" {
		line += "  " + sflByteStyle.Render(bytesText)
	}
	return line
}

func partAnnotOf(w sflog.IngestWorker) string {
	if w.PartsTotal > 1 {
		return fmt.Sprintf("(%d/%d)", w.PartIdx, w.PartsTotal)
	}
	return ""
}

// compactIngestArchiveName trims the sfu_ prefix and .txt.zst suffix from
// stamp-named archives so per-worker rows stay readable, mirroring sfu's
// compactArchiveName. Returns the base name unchanged on non-match.
func compactIngestArchiveName(path string) string {
	base := filepath.Base(path)
	if strings.HasPrefix(base, "sfu_") && strings.HasSuffix(base, ".txt.zst") {
		base = strings.TrimSuffix(strings.TrimPrefix(base, "sfu_"), ".txt.zst")
	}
	return base
}

// fitIngestLeft fits name + optional part annot into a fixed left column.
// The annot is dropped when it would leave no room for a name, so the cell
// never exceeds leftW and bars stay column-aligned.
func fitIngestLeft(name, partAnnot string, leftW int) (string, string) {
	if leftW < 1 {
		return "", ""
	}
	suffix := ""
	if partAnnot != "" {
		suffix = " " + partAnnot
	}
	if lipgloss.Width(name)+lipgloss.Width(suffix) <= leftW {
		return name, partAnnot
	}
	if lipgloss.Width(suffix) >= leftW {
		return fitIngestName(name, leftW), ""
	}
	return fitIngestName(name, leftW-lipgloss.Width(suffix)), partAnnot
}

// fitIngestName head-trims to max terminal cells, keeping the tail (part ids).
// It now delegates to the shared tuiframe helper (same observable behavior:
// ellipsis prefix, grapheme-safe, honors any positive budget) so the cell math
// stays in one place. Unlike the old TruncatePath it honors budgets below 8,
// so a tight regen column cannot overflow and let padOrTrim chop bars off the
// right edge.
func fitIngestName(name string, max int) string {
	if max < 1 {
		max = 1
	}
	return tuiframe.TruncateLeft(name, max)
}

// sfl worker-panel sizing. A small floor keeps the "many things at once" feel
// even on short terminals; the panel expands toward the worker count when there
// is vertical room, mirroring sfu's OD frame.
const (
	sflMaxWorkerRows = 8
	// widest stage label ("testing password"); the stage column is padded to
	// this so paths line up across rows.
	sflStageColW = 16
)

// sflWorkerStageLabel renders the panel label for one worker, falling back to
// the per-stage label. Width fits sflStageColW.
func sflWorkerStageLabel(w sflog.ActiveWorker) string {
	return sflStageLabel(w.Stage)
}

// sflStageLabel maps a worker stage to its user-facing panel label. It widens
// the short canonical names from WorkerStage.String() so the panel reads as
// intent rather than a bare verb: the credential actions name what they
// operate on ("extracting ulps", "parsing ulps"), while the
// archive-prep actions stay short ("opening", "testing password"). Every label
// fits the fixed sflStageColW (16) column — "testing password" is the widest
// at exactly 16 — so paths still align without a width
// change. The canonical String() in sflog is left intact for debug logs/tests.
func sflStageLabel(s sflog.WorkerStage) string {
	switch s {
	case sflog.StageOpening:
		return "opening"
	case sflog.StageTestingPassword:
		return "testing password"
	case sflog.StageExtracting:
		return "extracting ulps"
	case sflog.StageParsing:
		return "parsing ulps"
	default:
		return "working"
	}
}

// renderSflWorkerRow is one panel line: "[i] ⠹ <stage>  <path>". A braille
// spinner (phase-shifted by worker index for a gentle cascade) sits between the
// marker and the fixed-width stage column so paths still align; the path is
// truncated to fit. The stage label is the explicit TUI form via
// sflWorkerStageLabel.
func renderSflWorkerRow(w sflog.ActiveWorker, inner, idxMarkerW, tick int) string {
	marker := fmt.Sprintf("[%d]", w.Index+1)
	if pad := idxMarkerW - lipgloss.Width(marker); pad > 0 {
		marker += strings.Repeat(" ", pad)
	}
	stage := sflWorkerStageLabel(w)
	if pad := sflStageColW - lipgloss.Width(stage); pad > 0 {
		stage += strings.Repeat(" ", pad)
	}
	// reserve: marker + space + spinner(1) + space + stageCol + 2-space gap
	pathW := inner - idxMarkerW - 1 - 2 - sflStageColW - 2
	if pathW < 8 {
		pathW = 8
	}
	return sflMutedStyle.Render(marker) + " " +
		sflSpinnerStyle.Render(workerSpinnerFrame(tick, w.Index)) + " " +
		sflOkStyle.Render(stage) + "  " +
		sflMutedStyle.Render(tuiframe.TruncatePath(workerPathLabel(w.Path), pathW))
}

// termHeight is the terminal row count (stderr), defaulting to 24 when unknown
// so the worker panel still sizes sensibly on a non-TTY/redirected run.
func termHeight() int {
	_, h, err := term.GetSize(int(stderrFile.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// liveInterruptStatus is the live line shown at the top of the interrupt
// frame. It reports what's actually keeping the process alive after a
// graceful Ctrl-C, in order of what users perceive as slowest first:
//
//   - extract/parse workers still finishing,
//   - deferred temp-file removal,
//   - or nothing left to wait on.
//
// prog is *sflog.Progress (sfl's metrics), nil-safe so the cleanupLog
// count still surfaces when metrics aren't wired in (tests, early
// shutdown).
func liveInterruptStatus(prog *sflog.Progress, cleanupLog []string) string {
	if prog == nil {
		if n := len(cleanupLog); n > 0 {
			return fmt.Sprintf("Removing temp files — %d removed.", n)
		}
		return "Waiting for workers to finish…"
	}
	if n := len(prog.ActiveWorkers(999)); n > 0 {
		worker := "worker"
		if n != 1 {
			worker = "workers"
		}
		return fmt.Sprintf("Draining %d busy %s.", n, worker)
	}
	if n := len(cleanupLog); n > 0 {
		return fmt.Sprintf("Removing temp files — %d removed.", n)
	}
	return "Waiting for workers to finish…"
}

// renderInterrupt is the frame shown after a graceful Ctrl-C while in-flight
// reads finish and partial output is discarded.
//
// prog feeds a live status line so the user can tell the slow part
// (extract workers draining after cancel) from the
// fast part (deferred cleanup) from a hang — the old frame showed a
// static "Finishing in-flight reads" line with no signal.
func renderInterrupt(elapsed time.Duration, spinner string, prog *sflog.Progress, width int, cleanupLog []string) []string {
	header := headerLine(sflWarnStyle.Render(spinner), sflWarnStyle.Render("[!] INTERRUPTED — cleaning up"), elapsed, width)
	body := []string{
		liveInterruptStatus(prog, cleanupLog),
		"Finishing in-flight reads and discarding partial output.",
		"",
		sflMutedStyle.Render("A second Ctrl+C will force-exit immediately."),
	}
	var box []string
	if block := renderCleanupLogAbove(cleanupLog, termWidthFull()); len(block) > 0 {
		box = append(box, block...)
		box = append(box, "")
	}
	box = append(box, insetBox(sflInterruptBoxStyle, body, width)...)
	return frame(header, box, width)
}

// renderCleanupLogAbove is grey, full-terminal-width cleanup narration printed
// above the interrupt box.
func renderCleanupLogAbove(lines []string, width int) []string {
	if len(lines) == 0 {
		return nil
	}
	budget := width - sflLeftPad
	if budget < 8 {
		budget = 8
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, sflIndent+sflMutedStyle.Render(clampHead(ln, budget)))
	}
	return out
}

const (
	phaseDiscoverVal = 0 // mirrors sflog phaseDiscover
	phaseIngestVal   = 3 // mirrors sflog phaseIngest
	phaseDoneVal     = 4 // mirrors sflog phaseDone
)

// sflRecapLabelW is the label column width so recap values line up in a clean
// gutter (matches sfu's "Input "/"Unique "/"Removed " labels).
const sflRecapLabelW = 10

// recapRow is one "Label   value" line with the label padded to a fixed gutter
// so every value starts in the same column.
func recapRow(label, value string) string {
	if pad := sflRecapLabelW - lipgloss.Width(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	return sflLabelStyle.Render(label) + value
}

// envCapName formats an env-copy cap for recap display: MiB below 1 GiB,
// GiB at or above it, so the recap names the real limit from sflog.
func envCapName(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%g GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%g MiB", float64(n)/(1<<20))
}

// recapCountRows is the labeled metric block (one row per group so big counts
// never wrap mid-number). Field names and semantics mirror sfu's summary:
// Unique is what the library gained (the -o frame passes the written count,
// the -od frame passes the new-to-library count), and removedBullets groups
// the reasons parsed lines didn't land. Colors mirror sfu: cyan counts, gold
// bytes (via sflInsertOutputSizeRow), green unique, warn orange for problems.
func recapCountRows(stats sflog.ExtractStats, uniq int64, removedBullets []string, results []sflog.SourceResult) []string {
	dot := sflMutedStyle.Render("  ·  ")
	cred := int64(stats.Credentials)
	// F4 (#7): ArchivesScanned folds the archives found INSIDE other archives
	// into the same number as the ones the user passed — 2 passed, "206
	// archives" shown. When the run's per-source results are known, the Input
	// row counts sources from them directly: top-level archives are the
	// archive units. FilesScanned is NOT used here — it counts credential
	// files found INSIDE archives too, so the Input row would over-count
	// (2026-10-01). Without results (direct/test renders) the totals stand
	// alone, as before. The files segment is gone entirely (2026-10-03, user
	// decision): loose file sources are the rare case and "0 files" on
	// archive-only runs read as data loss.
	archives, nested := stats.ArchivesScanned, 0
	if len(results) > 0 {
		topLevel := 0
		for _, r := range results {
			if r.IsArchive {
				topLevel++
			}
		}
		archives = topLevel
		if nested = stats.ArchivesScanned - topLevel; nested < 0 {
			nested = 0
		}
	}
	inputVal := sflCountStyle.Render(formatInt(archives)) +
		sflMutedStyle.Render(" "+plural.Noun(archives, "archive", "archives"))
	if nested > 0 {
		inputVal += dot +
			sflCountStyle.Render(formatInt(nested)) +
			sflMutedStyle.Render(" nested")
	}
	rows := []string{
		recapRow("Input", inputVal),
	}
	if stats.HistoryChecked > 0 || stats.HistorySkipped > 0 {
		rows = append(rows, recapRow("History", sflCountStyle.Render(formatInt(stats.HistoryChecked))+
			sflMutedStyle.Render(" checked"+dot)+
			sflCountStyle.Render(formatInt(stats.HistorySkipped))+
			sflMutedStyle.Render(" skipped")))
	}
	rows = append(rows,
		recapRow("Lines", sflCountStyle.Render(formatInt(int(cred)))+sflMutedStyle.Render(" parsed")),
		recapRow("Unique", sflUniqueStyle.Render(formatInt(int(uniq)))+
			sflMutedStyle.Render(tuistat.ShareParen(uniq, cred))+
			sflMutedStyle.Render(" "+plural.Noun(int(uniq), "entry", "entries"))),
	)
	if len(removedBullets) > 0 {
		rows = append(rows, renderSflRemovedRows(removedBullets, boxInner(termWidth()))...)
	}
	// Failed/unopened sources are deliberately not a recap row: the issues
	// log and exit code carry them (2026-09-30, user).
	if stats.EnvCopied > 0 || stats.EnvDirsCopied > 0 {
		envVal := sflCountStyle.Render(formatInt(stats.EnvCopied)) +
			sflMutedStyle.Render(" copied")
		if stats.EnvDeduped > 0 {
			envVal += dot +
				sflCountStyle.Render(formatInt(stats.EnvDeduped)) +
				sflMutedStyle.Render(" dupes")
		}
		if stats.EnvDirsCopied > 0 {
			envVal += dot +
				sflCountStyle.Render(formatInt(stats.EnvDirsCopied)) +
				sflMutedStyle.Render(" "+plural.Noun(stats.EnvDirsCopied, "tdata folder", "tdata folders"))
		}
		rows = append(rows, recapRow("Envs", envVal))
	}
	if stats.EnvSkippedOverCap > 0 {
		rows = append(rows, recapRow("Envs skip", sflWarnStyle.Render(formatInt(stats.EnvSkippedOverCap))+
			sflMutedStyle.Render(" over "+envCapName(sflog.EnvCopyMaxLen)+" cap")))
	}
	if stats.EnvDirsSkippedOverCap > 0 {
		rows = append(rows, recapRow("tdata skip", sflWarnStyle.Render(formatInt(stats.EnvDirsSkippedOverCap))+
			sflMutedStyle.Render(" over "+envCapName(sflog.TdataCopyMaxBytes)+" cap")))
	}
	// -del transparency: what was destroyed, and what was deliberately kept
	// because it failed (the summary must never be silent about deletion).
	if stats.DeletedSources > 0 {
		rows = append(rows, recapRow("Deleted", sflCountStyle.Render(formatInt(stats.DeletedSources))+
			sflMutedStyle.Render(" source(s)")))
	}
	if stats.PreservedSources > 0 {
		rows = append(rows, recapRow("Preserved", sflWarnStyle.Render(formatInt(stats.PreservedSources))+
			sflMutedStyle.Render(" failed source(s)")))
	}
	return rows
}

// ingestRemovedBullets mirrors sfu's Removed row: rejected, in-run
// duplicates, already in library. Zero segments are omitted; shares are of
// parsed lines (sfu uses its accepted count for the same shares).
func ingestRemovedBullets(dropped, alreadyInLib, dup, cred int64) []string {
	var bullets []string
	if dropped > 0 {
		bullets = append(bullets, sflWarnStyle.Render(formatInt(int(dropped)))+
			sflMutedStyle.Render(tuistat.ShareParen(dropped, cred))+
			" "+sflMutedStyle.Render("rejected"))
	}
	if dup > 0 {
		bullets = append(bullets, sflCountStyle.Render(formatInt(int(dup)))+
			sflMutedStyle.Render(tuistat.ShareParen(dup, cred))+
			" "+sflMutedStyle.Render("dup"))
	}
	if alreadyInLib > 0 {
		bullets = append(bullets, sflCountStyle.Render(formatInt(int(alreadyInLib)))+
			sflMutedStyle.Render(tuistat.ShareParen(alreadyInLib, cred))+
			" "+sflMutedStyle.Render("already in library"))
	}
	return bullets
}

// renderSflRemovedRows mirrors sfu's Removed recap: one line when it fits, else
// stacked bullets under the label. Continuations drop the label indent when
// indent+bullet would still overflow the box (same pattern as Merge).
func renderSflRemovedRows(bullets []string, maxInnerWidth int) []string {
	if len(bullets) == 0 {
		return nil
	}
	// Pad to the same gutter recapRow uses so the value column lines up with
	// Added/Unique/etc. instead of starting one cell short.
	label := "Removed"
	if pad := sflRecapLabelW - lipgloss.Width(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	sep := sflMutedStyle.Render(" · ")
	singleLineRest := strings.Join(bullets, sep)
	totalWidth := lipgloss.Width(label) + lipgloss.Width(singleLineRest)
	if maxInnerWidth <= 0 || totalWidth <= maxInnerWidth {
		return []string{sflLabelStyle.Render(label) + singleLineRest}
	}
	indent := strings.Repeat(" ", lipgloss.Width(label))
	fits := func(s string) bool {
		return lipgloss.Width(s) <= maxInnerWidth
	}
	first := sflLabelStyle.Render(label) + bullets[0]
	if !fits(first) {
		first = bullets[0]
	}
	rows := []string{first}
	for _, b := range bullets[1:] {
		cont := indent + b
		if !fits(cont) {
			cont = b
		}
		rows = append(rows, cont)
	}
	return rows
}

// renderIngestLibraryRows shows what the ingest did with the extracted uniques:
// how many were added, then a "Removed" row grouping the reasons a unique didn't
// land -- mirroring sfu's renderDoneLines. The rejected count leads in warn
// style (the actionable "the library refused these" number), then library hits.
// Extraction already deduped, so there are no within-run duplicates to show
// here (sfu's third bullet). Surfacing rejected closes the recap's arithmetic:
// extraction Unique == Added + rejected + already-in-library.
func renderIngestLibraryRows(newToLib, alreadyInLib, dropped, emitted int64, innerWidth int, dryRun bool) []string {
	addedLabel := "Added"
	if dryRun {
		addedLabel = "Would add"
	}
	rows := []string{recapRow(addedLabel, sflUniqueStyle.Render(formatInt(int(newToLib)))+
		sflMutedStyle.Render(tuistat.ShareParen(newToLib, emitted))+
		sflMutedStyle.Render(" "+plural.Noun(int(newToLib), "entry", "entries")))}
	var bullets []string
	if dropped > 0 {
		bullets = append(bullets, sflWarnStyle.Render(formatInt(int(dropped)))+
			sflMutedStyle.Render(tuistat.ShareParen(dropped, emitted))+
			" "+sflMutedStyle.Render("rejected"))
	}
	if alreadyInLib > 0 {
		bullets = append(bullets, sflCountStyle.Render(formatInt(int(alreadyInLib)))+
			sflMutedStyle.Render(tuistat.ShareParen(alreadyInLib, emitted))+
			" "+sflMutedStyle.Render("already in library"))
	}
	if len(bullets) > 0 {
		rows = append(rows, renderSflRemovedRows(bullets, innerWidth)...)
	}
	return rows
}

// dryRunTitle is the only title a summary frame still carries: dry-run is a
// mode indicator, not an outcome warning, so preview frames announce DRY RUN.
// Outcome titles (COMPLETE/INGESTED, ✓/⚠ marks, FAILED/HISTORY suffixes) were
// cut — the summary leads with the box, and outcome state lives in the recap
// rows, the issues log, and the exit code (internal/exitcode).
func dryRunTitle() string {
	return sflIndent + sflOkStyle.Render("✓ ") + sflTitleStyle.Render("SnowFastLog DRY RUN")
}

func renderFinalSummary(outPath string, stats sflog.ExtractStats) []string {
	return renderFinalSummaryWithNotice(outPath, stats, nil, nil)
}

// renderFinalSummaryWithNotice is the classic -o completion frame: the recap
// box, then the output-path footer and footer lines. The area above the box is
// blank; non-dry-run frames carry no title line at all. results carries the
// run's per-source outcomes so the recap Input row can split top-level from
// nested archives (nil falls back to the plain totals).
func renderFinalSummaryWithNotice(outPath string, stats sflog.ExtractStats, notice *selfupdate.Notice, results []sflog.SourceResult) []string {
	width := termWidth()
	body := recapCountRows(stats, int64(stats.Emitted),
		ingestRemovedBullets(0, 0, int64(stats.Duplicates), int64(stats.Credentials)), results)
	if outPath != "" {
		body = sflInsertOutputSizeRow(body, sflDiskBytes([]string{outPath}))
	}
	box := sflGradientBox(body, width, gradStart, gradEnd)
	out := frameWithFooter("", box, width, nil)
	if outPath != "" {
		out = append(out, renderSflPathFooter("Output   ", []string{outPath}, sflOkStyle)...)
	}
	return append(out, summaryFooterLines(width, notice)...)
}

// renderNoIngestSummary is the -od frame when extraction produced no
// credentials: a calm "done, nothing to do" recap with the library left
// untouched, rather than an error exit.
func renderNoIngestSummary(libraryDir string, stats sflog.ExtractStats, dryRun bool) []string {
	return renderNoIngestSummaryWithNotice(libraryDir, stats, nil, dryRun, nil)
}

// renderNoIngestSummaryWithNotice is renderNoIngestSummary with an optional
// self-update footer notice. Dry-run frames keep the DRY RUN title; other
// frames start straight at the box.
func renderNoIngestSummaryWithNotice(libraryDir string, stats sflog.ExtractStats, notice *selfupdate.Notice, dryRun bool, results []sflog.SourceResult) []string {
	width := termWidth()
	title := ""
	if dryRun {
		title = dryRunTitle()
	}
	body := append(recapCountRows(stats, int64(stats.Emitted),
		ingestRemovedBullets(0, 0, int64(stats.Duplicates), int64(stats.Credentials)), results),
		"",
		"", // the "No credentials extracted — library unchanged." line was cut (2026-10-03, user request); the blank row keeps the box height
	)
	box := sflGradientBox(body, width, gradStart, gradEnd)
	out := frameWithFooter(title, box, width, nil)
	// Library path footer cut (2026-10-01, user request) — same as the ingest
	// summary; the user knows where their library is.
	return append(out, summaryFooterLines(width, notice)...)
}

// renderIngestSummary is the -od completion frame: the same extraction recap,
// what this run contributed (new vs already-present), and the resulting library
// line count and path, so the single icy frame ends the run instead of handing
// off to sfu's summary.
func renderIngestSummary(libraryDir string, libraryLines, newToLib, alreadyInLib, dropped int64, stats sflog.ExtractStats, outputPaths []string, dryRun bool) []string {
	return renderIngestSummaryWithNotice(libraryDir, libraryLines, newToLib, alreadyInLib, dropped, stats, outputPaths, nil, dryRun, nil)
}

// renderIngestSummaryWithNotice is renderIngestSummary with an optional
// self-update footer notice. Dry-run frames keep the DRY RUN title; other
// frames start straight at the box.
func renderIngestSummaryWithNotice(libraryDir string, libraryLines, newToLib, alreadyInLib, dropped int64, stats sflog.ExtractStats, outputPaths []string, notice *selfupdate.Notice, dryRun bool, results []sflog.SourceResult) []string {
	width := termWidth()
	title := ""
	if dryRun {
		title = dryRunTitle()
	}
	// Box holds clean stats only: extraction recap + Added/Removed ingest rows.
	// Library/Output paths live in outside-box footers (full width, never
	// padOrTrim'd). Failures/skips go to the -err file; the running library
	// total gets its own box below (mirrors sfu's renderODSummary).
	body := recapCountRows(stats, newToLib,
		ingestRemovedBullets(dropped, alreadyInLib, int64(stats.Duplicates), int64(stats.Credentials)), results)
	if !dryRun {
		body = sflInsertOutputSizeRow(body, sflDiskBytes(outputPaths))
	}
	box := sflGradientBox(body, width, gradStart, gradEnd)
	box = append(box, "")
	box = append(box, libraryTotalBox(libraryLines, width)...)
	out := frameWithFooter(title, box, width, nil)
	// The Library path footer was cut (2026-10-01, user request): the user
	// configures the library and doesn't need it echoed back. Output keeps
	// its footer (renderIngestOutputFooter).
	out = append(out, renderIngestOutputFooter(outputPaths, dryRun)...)
	out = append(out, summaryFooterLines(width, notice)...)
	return out
}

// sflDiskBytes sums on-disk sizes for paths that exist. Missing paths contribute
// nothing so a not-yet-written test path does not invent an Output size row.
func sflDiskBytes(paths []string) int64 {
	var n int64
	for _, p := range paths {
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		n += fi.Size()
	}
	return n
}

// sflInsertOutputSizeRow inserts a gold Output size row after Lines when
// diskBytes > 0, matching sfu's DONE summary byte role.
func sflInsertOutputSizeRow(rows []string, diskBytes int64) []string {
	if diskBytes <= 0 {
		return rows
	}
	outRow := recapRow("Output", sflByteStyle.Render(humanBytes(diskBytes)))
	linesPrefix := recapRow("Lines", "")
	for i, r := range rows {
		if !strings.HasPrefix(r, linesPrefix) {
			continue
		}
		out := make([]string, 0, len(rows)+1)
		out = append(out, rows[:i+1]...)
		out = append(out, outRow)
		out = append(out, rows[i+1:]...)
		return out
	}
	return append(rows, outRow)
}

// renderSflPathFooter lays out a labelled, gradient-bordered list of paths under
// a summary box (same gutter as sfu's renderDonePathFooter). Paths are not
// padOrTrim'd so long mounts stay readable.
func renderSflPathFooter(label string, paths []string, pathStyle lipgloss.Style) []string {
	if len(paths) == 0 {
		return nil
	}
	mid := gradStart.BlendLuv(gradEnd, 0.5)
	border := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	labelCell := sflLabelStyle.Render(label)
	labelW := lipgloss.Width(labelCell)
	prefix := strings.Repeat(" ", sflLeftPad) + border.Render("┃") + "  "
	blankLabel := strings.Repeat(" ", labelW)

	out := []string{""}
	for i, p := range paths {
		pathCell := pathStyle.Render(pathdisp.ForDisplay(p))
		var line string
		if i == 0 {
			line = prefix + labelCell + pathCell
		} else {
			line = prefix + blankLabel + pathCell
		}
		out = append(out, line)
	}
	return out
}

// renderIngestOutputFooter lists archive(s) written this run below the ingest
// summary boxes, mirroring sfu's renderDoneOutputFooter layout.
func renderIngestOutputFooter(paths []string, dryRun bool) []string {
	const label = "Output   "
	mid := gradStart.BlendLuv(gradEnd, 0.5)
	border := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	labelCell := sflLabelStyle.Render(label)
	prefix := strings.Repeat(" ", sflLeftPad) + border.Render("┃") + "  "

	// dry-run wrote nothing to the library; state that plainly instead of
	// listing the per-run temp scratch paths (already cleaned by now).
	if dryRun {
		return []string{"", prefix + labelCell + sflMutedStyle.Render("(dry run — nothing written)")}
	}

	// Empty after a completed ingest = nothing new was added (all duplicates); the
	// engine discarded the empty shard, so state that plainly instead of dropping
	// the row (which would leave the user wondering where the output went).
	if len(paths) == 0 {
		return []string{"", prefix + labelCell + sflMutedStyle.Render("(nothing new)")}
	}

	return renderSflPathFooter(label, paths, sflOkStyle)
}

// libraryTotalBox is the standalone "<N> lines in library" box, the single
// headline number after ingestion (prior library + new unique this run).
func libraryTotalBox(libraryLines int64, width int) []string {
	body := []string{
		sflUniqueStyle.Render(formatInt(int(libraryLines))),
		sflMutedStyle.Render("lines in library"),
	}
	return sflGradientBox(body, width, gradStart, gradEnd)
}

// renderInterruptSummary is printed on the normal screen after a graceful
// Ctrl-C, replacing a bare "interrupted" line with a styled notice so the exit
// reads as deliberate rather than a crash.
func renderInterruptSummary(elapsed time.Duration, cleanupLog []string) []string {
	width := termWidth()
	title := sflIndent + sflWarnStyle.Render("⚠ SnowFastLog INTERRUPTED")
	body := []string{
		"Stopped before completion — partial output discarded.",
		sflMutedStyle.Render(fmt.Sprintf("Ran for %s · re-run to start over.", formatDuration(elapsed))),
	}
	var box []string
	if block := renderCleanupLogAbove(cleanupLog, termWidthFull()); len(block) > 0 {
		box = append(box, block...)
		box = append(box, "")
	}
	box = append(box, insetBox(sflInterruptBoxStyle, body, width)...)
	return frame(title, box, width)
}

// clampHead trims s to at most max terminal cells, keeping the start (the file
// name) and marking the cut with an ellipsis, so a pathological member name
// can't soft-wrap the muted block across the terminal. Truncation goes through
// the shared helper so ANSI and grapheme clusters survive intact.
func clampHead(s string, max int) string {
	if max < 1 {
		max = 1
	}
	if tuiframe.VisibleWidth(s) <= max {
		return s
	}
	return tuiframe.TruncateRight(s, max-1) + "…"
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// workerPathLabel renders a worker slot's current archive for the live row.
// While a worker is inside a nested archive the engine stores the raw
// provenance ("outer.rar!sub/inner.7z"); collapse it to "outer ▸ inner" so the
// line names the archive actually being worked, not just the top-level item.
// Non-nested paths (no "!") are returned unchanged for the caller to truncate
// as an ordinary path tail.
func workerPathLabel(p string) string {
	first := strings.IndexByte(p, '!')
	if first < 0 {
		return p
	}
	outer := baseName(p[:first])
	inner := baseName(p[strings.LastIndexByte(p, '!')+1:])
	return outer + " ▸ " + inner
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanBytes is sfu's spaced form ("12.0 GB"), used by the ingest stage's
// Bytes/Throughput/System rows so both CLIs read identically there.
// Rolls to the next unit at 1000 (not 1024) so display never exceeds 8 chars.
func humanBytes(n int64) string {
	if n < 0 {
		return "0 B"
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	u := 0
	for v >= 1000 && u < len(units)-1 {
		v /= 1024
		u++
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}

// formatRate mirrors sfu's formatRate for the ingest Throughput row.
func formatRate(bps float64) string {
	if bps <= 0 {
		return "0 B/s"
	}
	if bps < 1024 {
		return fmt.Sprintf("%.0f B/s", bps)
	}
	return humanBytes(int64(bps)) + "/s"
}

// ingest-stage fixed columns, mirroring sfu's byte/rate slots.
const (
	sflBytesColWidth = 8 // "999.9 GB"

	// sflIngestStatLabelColWidth mirrors sfu's statLabelColWidth: the
	// Lines/Progress/System/Library labels inside the ingest boxes all
	// pad to 13 cells so values align across rows.
	sflIngestStatLabelColWidth = 13
)

// sflStatLabel renders a stat row label padded to the 13-cell ingest column
// (sfu's statLabel, sfl palette).
func sflStatLabel(name string) string {
	s := sflLabelStyle.Render(name)
	if w := lipgloss.Width(s); w < sflIngestStatLabelColWidth {
		return s + strings.Repeat(" ", sflIngestStatLabelColWidth-w)
	}
	return s
}

// sflSystemRow mirrors sfu's renderSystemRow ("System   RAM …  CPU …").
func sflSystemRow(ramMB, cpuPct float64) string {
	return sflStatLabel("System") +
		"RAM " + sflByteStyle.Render(padLeft(humanBytes(int64(ramMB*1024*1024)), sflBytesColWidth)) + "    " +
		"CPU " + sflCountStyle.Render(fmt.Sprintf("%4.0f%%", cpuPct))
}

// sflIngestNumber aligns a counter to nDigits so values stack by magnitude.
func sflIngestNumber(n int64, nDigits int) string {
	return padLeft(formatInt(int(n)), nDigits)
}

// sflNumDigits is the width fmt would use for n in base 10.
func sflNumDigits(n int64) int {
	if n == 0 {
		return 1
	}
	d := 0
	if n < 0 {
		d = 1
		n = -n
	}
	for n > 0 {
		d++
		n /= 10
	}
	return d
}

func formatInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

// formatLibraryCount is the compact badge form (3.29B) so the ingest
// Library stat row can carry the relocated library total without eating
// the box width.
func formatLibraryCount(n int64) string {
	if n < 0 {
		return "0"
	}
	if n < 1_000_000 {
		return formatInt(int(n))
	}
	units := []string{"", "K", "M", "B", "T"}
	v := float64(n)
	u := 0
	for v >= 1000 && u < len(units)-1 {
		v /= 1000
		u++
	}
	if v >= 100 {
		return fmt.Sprintf("%.0f%s", v, units[u])
	}
	if v >= 10 {
		return fmt.Sprintf("%.1f%s", v, units[u])
	}
	return fmt.Sprintf("%.2f%s", v, units[u])
}

func padLeft(s string, w int) string {
	n := lipgloss.Width(s)
	if n >= w {
		return s
	}
	return strings.Repeat(" ", w-n) + s
}
