package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
	"github.com/snowx-dev/SnowFastULP/internal/tuistat"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
)

const (
	leftPad   = 4
	indentStr = "    "

	// right-aligned tagline, shared ice-blue footer gradient spans both lines
	tuiFooterLine1 = "sfs is open-source ❤️"
	tuiFooterLine2 = "https://snowx.dev"
)

// Horizontal layout: the content block is indented leftPad on the LEFT and the
// same on the RIGHT so it sits balanced in the terminal rather than flush
// against the right edge.
//
//	contentWidth  — outer width of the bordered box / bar region.
//	boxInnerWidth — usable text width inside gradientBox (2 borders + 4 padding).
func contentWidth(width int) int  { return width - 2*leftPad }
func boxInnerWidth(width int) int { return contentWidth(width) - 6 }

var (
	phaseStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "71"})
	labelStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
	mutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
	timeStyle    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "179"})
	countStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "94", Dark: "209"})
	hitStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})
	byteStyle    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "178", Dark: "216"})
	spinnerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "179"})
	emptyStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "250", Dark: "238"})
)

// sfs owns a PEACH identity: warm orange-peach ramps, clearly distinct from
// sfu's purple (#5A56E0→#EE6FF8) and sfl's plum (#9E3A6E→#DD2E44) at a
// glance. Footer gradient + heart red stay repo-wide; warn semantics keep
// their amber→red pair.
var (
	// box/bar gradient, search + done: peach → light apricot
	gradStart, _ = colorful.Hex("#E8703A")
	gradEnd, _   = colorful.Hex("#F6C8A8")

	// footer taglines, ice blue → icy white (shared repo-wide)
	footerGradA, _ = colorful.Hex("#7DD3E8")
	footerGradB, _ = colorful.Hex("#F2F8FC")

	// box frames / progress bars while indexing, warm sand → cream
	// (same lightness arc as sfu's frost pair, peach hue)
	frostGradA, _ = colorful.Hex("#D97742")
	frostGradB, _ = colorful.Hex("#FBE3CD")

	// open-source heart in the footer, bright red ❤️ (shared repo-wide)
	heartRed = lipgloss.Color("#FF2B2B")

	// interrupt frame, amber → muted red (warn semantics, shared)
	interruptStart, _ = colorful.Hex("#E0B040")
	interruptEnd, _   = colorful.Hex("#C04030")
)

// fixed label col so percent suffixes align across bars
const progressBarLabelWidth = 9 // "Scanned  "

var interruptWarnStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})

var spinnerFrames = []string{"|", "/", "-", "\\"}

type uiMode int

const (
	uiSilent uiMode = iota
	uiFull
)

type uiConfig struct {
	Mode     uiMode
	Metrics  *search.Metrics
	Pattern  string
	Start    time.Time
	Done     <-chan struct{}
	Signaled func() bool
}

// renderQueryLine formats the "Query" panel row, truncating the pattern to fit.
// Returns "" for an empty pattern so callers can omit the row entirely.
func renderQueryLine(pattern string, innerW int) string {
	if pattern == "" {
		return ""
	}
	display := pattern
	if pattern == "*" {
		display = "* (all lines)"
	}
	max := innerW - 16
	if max < 8 {
		max = 8
	}
	return labelStyle.Render("Query     ") + byteStyle.Render("\""+trimToDisplayWidth(display, max)+"\"")
}

// stderrIsTTY reports whether stderr is a terminal; a var so tests can force
// TTY mode for the panic-teardown seam.
var stderrIsTTY = func() bool {
	fi, err := os.Stderr.Stat()
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

// drawPanicHook is a test seam: when non-nil, the runUI goroutine's draw path
// calls it right after a frame render, so tests can inject a panic after the
// alt screen went up and prove the terminal is restored before the crash
// surfaces. Production never sets it.
var drawPanicHook func()

// applyStderrColorProfile downgrades lipgloss to plain ASCII when stderr is not
// a terminal. The live screen and any styled status render to stderr, so a
// redirected stderr (even with stdout a TTY) must stay free of ANSI escapes.
// Mirrors sfl/sfu.
func applyStderrColorProfile() {
	if !stderrIsTTY() {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

func uiModeString(m uiMode) string {
	switch m {
	case uiSilent:
		return "silent"
	case uiFull:
		return "full"
	default:
		return "unknown"
	}
}

func resolveUIMode(silent bool) uiMode {
	if silent || !stderrIsTTY() {
		return uiSilent
	}
	return uiFull
}

// resolveRunUIMode picks the sfs surface. The default is silent. -stats
// requests the live screen, and ShowLiveDisplay may still refuse it.
// jsonTarget is "", "-", or a file path. stdoutTTY is whether stdout is a
// terminal. stderr is read from stderrIsTTY.
func resolveRunUIMode(statsMode bool, jsonTarget string, stdoutTTY, vtOK bool) uiMode {
	if !statsMode {
		return uiSilent
	}
	if tuistat.ShowLiveDisplay(false, jsonTarget, stderrIsTTY(), stdoutTTY, vtOK) {
		return uiFull
	}
	return uiSilent
}

func runUI(cfg uiConfig, done *sync.WaitGroup) {
	if done != nil {
		defer done.Done()
	}
	if cfg.Mode == uiSilent {
		<-cfg.Done
		return
	}

	frame := stderrFrame{tty: stderrIsTTY()}
	reg.Set(frame.close)
	defer reg.Clear()
	defer frame.close()
	// Registered last so it runs first: a panic in the render/teardown path
	// restores the terminal through the registry before the goroutine dies,
	// then re-panics so the crash still surfaces.
	defer restoreOnPanic()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var rates rateTracker

	for {
		select {
		case <-cfg.Done:
			return
		case now := <-ticker.C:
			if cfg.Signaled != nil && cfg.Signaled() {
				frame.draw(renderInterrupt(now.Sub(cfg.Start), termWidth(), ulpengine.SnapshotCleanupLog()))
				if drawPanicHook != nil {
					drawPanicHook()
				}
				continue
			}
			curRates := rates.sample(now, cfg.Metrics)
			switch cfg.Mode {
			case uiFull:
				frame.draw(renderFull(now, cfg.Start, cfg.Metrics, curRates, cfg.Pattern))
				if drawPanicHook != nil {
					drawPanicHook()
				}
			}
		}
	}
}

// stderrFrame is a fixed alt-screen status block redrawn in place each tick.
// Draw and close are serialized so a teardown (signal/fatal goroutine) can
// never interleave between line writes and spill the frame onto the primary
// screen. close is idempotent, and once closed the frame never re-enters the
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
	fmt.Fprint(os.Stderr, termctl.ANSIResetScroll+termctl.ANSIShowCursor+termctl.AltScreenLeave)
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
	width := termWidth()
	rows := make([]string, len(lines))
	for i, ln := range lines {
		rows[i] = trimToDisplayWidth(ln, width)
	}
	var b strings.Builder
	if !f.altOn {
		b.WriteString(termctl.AltScreenEnter + termctl.ANSIHideCursor)
		f.altOn = true
	}
	// Clamp to one row shy of the terminal height so the bottom line can't
	// scroll the buffer; Compose erases any rows a taller previous frame left.
	// A <=1-row terminal would make that budget 0/negative, which Compose
	// treats as UNLIMITED (full-frame spam every tick) — clamp to one row
	// instead: never pass a nonpositive maxRows (P5-W9).
	maxRows := termHeight() - 1
	if maxRows < 1 {
		maxRows = 1
	}
	b.WriteString(tuiframe.Compose(rows, maxRows))
	fmt.Fprint(os.Stderr, b.String())
}

func renderHeader(left, right string, width int) string {
	pad := (width - leftPad) - tuiVisibleWidth(left) - tuiVisibleWidth(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

func renderInterrupt(elapsed time.Duration, width int, cleanupLog []string) []string {
	header := renderHeader(
		interruptWarnStyle.Render("[!]")+"  "+phaseStyle.Render("INTERRUPTED — cleaning up"),
		timeStyle.Render(formatElapsed(elapsed)),
		width,
	)
	out := []string{"", header}
	if block := renderCleanupLogAbove(cleanupLog, termWidthFull()); len(block) > 0 {
		out = append(out, "")
		out = append(out, block...)
	}
	inner := []string{
		"Stopping index and search workers.",
		"This usually takes a few seconds; please wait.",
		"",
		mutedStyle.Render("A second Ctrl+C will force-exit immediately."),
	}
	box := gradientBox(inner, contentWidth(width), interruptStart, interruptEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")
	out = append(out, "")
	out = append(out, boxLines...)
	return append(out, renderLiveScreenFooter(width)...)
}

// renderCleanupLogAbove is grey, full-terminal-width cleanup narration printed
// above the interrupt box.
func renderCleanupLogAbove(lines []string, width int) []string {
	if len(lines) == 0 {
		return nil
	}
	pad := strings.Repeat(" ", leftPad)
	budget := width - leftPad
	if budget < 8 {
		budget = 8
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, pad+trimToDisplayWidth(mutedStyle.Render(ln), budget))
	}
	return out
}

func renderFull(now, start time.Time, m *search.Metrics, rates uiRates, pattern string) []string {
	return renderFullAt(now, start, m, rates, pattern, termWidth())
}

func renderFullAt(now, start time.Time, m *search.Metrics, rates uiRates, pattern string, width int) []string {
	contentW := contentWidth(width)
	boxInner := boxInnerWidth(width)
	phase := m.Phase.Load()
	_, phaseLabel, boxStart, boxEnd, barStart, barEnd := phaseVisuals(phase, m)

	elapsed := now.Sub(start)
	spinner := spinnerStyle.Render(spinnerFrames[(now.UnixMilli()/100)%int64(len(spinnerFrames))])
	header := renderHeader(
		indentStr+spinner+"  "+phaseStyle.Render("[sfs] "+phaseLabel),
		timeStyle.Render(formatElapsed(elapsed)),
		width,
	)

	archDone := m.ArchivesIndexed.Load()
	archActive := m.IndexArchivesActive.Load()
	if phase >= search.PhaseSearch {
		archDone = m.ArchivesDone.Load()
	}
	archTotal := m.ArchivesTotal.Load()
	hits := m.Hits.Load()

	inner := []string{renderThroughputRow(phase, rates), renderETARow(phase, rates)}
	if q := renderQueryLine(pattern, boxInner); q != "" {
		inner = append(inner, q)
	}
	inner = append(inner,
		labelStyle.Render("Archives  ")+countStyle.Render(fmt.Sprintf("%d", archDone))+
			mutedStyle.Render(" / ")+countStyle.Render(fmt.Sprintf("%d", archTotal)),
	)
	if phase == search.PhaseIndex && archActive > 0 {
		inner[len(inner)-1] += mutedStyle.Render(fmt.Sprintf("  (%d active)", archActive))
	}
	if phase == search.PhaseIndex {
		done, total := indexBytes(m)
		inner = append(inner, labelStyle.Render("Index     ")+byteStyle.Render(formatBytes(done))+
			mutedStyle.Render(" / ")+byteStyle.Render(formatBytes(total)))
	} else {
		chunkProgress, chunkTotal := searchChunkProgress(m)
		chunkDigits := numDigits(chunkTotal)
		inner = append(inner,
			labelStyle.Render("Chunks    ")+countStyle.Render(fmt.Sprintf("%*.1f", chunkDigits+2, chunkProgress))+
				mutedStyle.Render(" / ")+countStyle.Render(fmt.Sprintf("%d", chunkTotal)),
			labelStyle.Render("Hits      ")+hitStyle.Render(fmt.Sprintf("%d", hits)),
		)
		scannedDone, scannedTotal := searchBytes(m)
		inner = append(inner, labelStyle.Render("Scanned   ")+byteStyle.Render(formatBytes(scannedDone))+
			mutedStyle.Render(" / ")+byteStyle.Render(formatBytes(scannedTotal)))
		inner = append(inner, renderLibrarySizeRow(m))
	}

	box := gradientBox(inner, contentW, boxStart, boxEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")
	barLines := renderLabeledProgressBars(phase, m, contentW, barStart, barEnd)

	// header, box, bar1, bar2, frost tagline footer (blanks between)
	out := []string{header, ""}
	out = append(out, boxLines...)
	out = append(out, "", barLines[0], "", barLines[1])
	out = append(out, renderLiveScreenFooter(width)...)
	return out
}

func renderFinalSummary(start time.Time, m *search.Metrics, outFile, pattern string, notice *selfupdate.Notice) []string {
	width := termWidth()
	contentW := contentWidth(width)
	boxInner := boxInnerWidth(width)
	elapsed := time.Since(start)
	scannedDone, scannedTotal := searchBytes(m)

	inner := []string{}
	if q := renderQueryLine(pattern, boxInner); q != "" {
		inner = append(inner, q)
	}
	inner = append(inner,
		labelStyle.Render("Hits      ")+hitStyle.Render(formatCount(m.Hits.Load())),
		labelStyle.Render("Elapsed   ")+timeStyle.Render(formatElapsed(elapsed)),
		labelStyle.Render("Archives  ")+countStyle.Render(formatCount(m.ArchivesDone.Load()))+
			mutedStyle.Render(" / ")+countStyle.Render(formatCount(m.ArchivesTotal.Load())),
		labelStyle.Render("Chunks    ")+countStyle.Render(formatCount(m.ChunksDone.Load()))+
			mutedStyle.Render(" / ")+countStyle.Render(formatCount(m.ChunksTotal.Load())),
		labelStyle.Render("Scanned   ")+byteStyle.Render(formatBytes(scannedDone))+
			mutedStyle.Render(" / ")+byteStyle.Render(formatBytes(scannedTotal)),
		renderLibrarySizeRow(m),
	)
	box := gradientBox(inner, contentW, gradStart, gradEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")
	// Exit-code policy twin (internal/exitcode): a capped chunk truncated the
	// results — the title must not read as a plain success while the process
	// exits 3.
	terminal := "✓ COMPLETE"
	if m.ChunksCapped.Load() > 0 {
		terminal = "✓ COMPLETE · TRUNCATED"
	}
	out := append([]string{phaseStyle.Render(terminal), ""}, boxLines...)
	if footer := renderOutputFooter(outFile, gradStart, gradEnd); len(footer) > 0 {
		out = append(out, footer...)
	}
	return append(out, renderSummaryFooter(width, notice)...)
}

// incompleteStyle colors the INCOMPLETE terminal line like an error: some
// file/chunk could not be scanned, so the run must never read as COMPLETE.
var incompleteStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "203"})

// renderIncompleteSummary is renderFinalSummary for a partial scan: the
// terminal line says INCOMPLETE (never COMPLETE) while still reporting the
// hits that WERE found before the failure.
func renderIncompleteSummary(start time.Time, m *search.Metrics, outFile, pattern string, notice *selfupdate.Notice) []string {
	out := renderFinalSummary(start, m, outFile, pattern, notice)
	out[0] = incompleteStyle.Render("✗ INCOMPLETE")
	return out
}

// formatElapsed renders the elapsed timer: below 10s one decimal keeps
// sub-second runs honest ("0.4s" instead of a misleading "0s"); above, whole
// seconds match the Go duration shape the frame already used ("42s",
// "1m30s").
func formatElapsed(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Truncate(time.Second).String()
}

func formatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	var b strings.Builder
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func gradientBarWithPercent(percent float64, width int, start, end colorful.Color) string {
	const suffixWidth = 7
	if width < suffixWidth+2 {
		width = suffixWidth + 2
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	body := width - suffixWidth
	fill := int(math.Round(float64(body) * percent))
	if fill > body {
		fill = body
	}
	var b strings.Builder
	for i := 0; i < fill; i++ {
		t := 0.0
		if body > 1 {
			t = float64(i) / float64(body-1)
		}
		c := start.BlendLuv(end, t)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render("█"))
	}
	if rem := body - fill; rem > 0 {
		b.WriteString(emptyStyle.Render(strings.Repeat("░", rem)))
	}
	b.WriteString(mutedStyle.Render(fmt.Sprintf(" %5.1f%%", percent*100)))
	return b.String()
}

func gradientBar(percent float64, width int) string {
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
	var b strings.Builder
	for i := 0; i < fill; i++ {
		t := 0.0
		if width > 1 {
			t = float64(i) / float64(width-1)
		}
		c := gradStart.BlendLuv(gradEnd, t)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render("█"))
	}
	if rem := width - fill; rem > 0 {
		b.WriteString(emptyStyle.Render(strings.Repeat("░", rem)))
	}
	return b.String()
}

func gradientBox(innerLines []string, outerWidth int, start, end colorful.Color) string {
	if outerWidth < 8 {
		outerWidth = 8
	}
	inner := outerWidth - 6
	if inner < 1 {
		inner = 1
	}
	mid := start.BlendLuv(end, 0.5)
	midStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	buildBorder := func(left, right string) string {
		var b strings.Builder
		for i := 0; i < outerWidth; i++ {
			t := 0.0
			if outerWidth > 1 {
				t = float64(i) / float64(outerWidth-1)
			}
			c := start.BlendLuv(end, t)
			ch := "─"
			switch i {
			case 0:
				ch = left
			case outerWidth - 1:
				ch = right
			}
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render(ch))
		}
		return b.String()
	}
	rows := make([]string, 0, len(innerLines)+2)
	rows = append(rows, buildBorder("╭", "╮"))
	for _, ln := range innerLines {
		rows = append(rows, midStyle.Render("│")+"  "+padOrTrim(ln, inner)+"  "+midStyle.Render("│"))
	}
	rows = append(rows, buildBorder("╰", "╯"))
	return strings.Join(rows, "\n")
}

func indentBlock(s string, pad int) string {
	prefix := strings.Repeat(" ", pad)
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

// renderFrostTagline applies a faint ice gradient per rune.
//
// The open-source heart (❤ + optional VS16) is styled as one grapheme so
// width libraries and terminals agree; splitting SGR across the cluster
// under-counts by one cell and soft-wraps junk onto the next row.
func renderFrostTagline(text string, spanStart, spanEnd float64) string {
	run := []rune(text)
	if len(run) == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(run); i++ {
		r := run[i]
		// bright red heart cluster, not the frost ramp
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
		} else if spanEnd != spanStart {
			t = (spanStart + spanEnd) / 2
		}
		c := footerGradA.BlendLuv(footerGradB, t)
		st := lipgloss.NewStyle().
			Foreground(lipgloss.Color(c.Hex())).
			Faint(true)
		b.WriteString(st.Render(string(r)))
	}
	return b.String()
}

func renderFrostTaglineRight(text string, width int, spanStart, spanEnd float64) string {
	styled := renderFrostTagline(text, spanStart, spanEnd)
	if width <= 0 {
		// A nonpositive footer budget gets no tagline: repeating spaces at a
		// negative count would panic and a positive-width repeat could only
		// widen the row.
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

func renderUpdateNoticeLine(notice *selfupdate.Notice) string {
	return interruptWarnStyle.Render("Update available: v"+notice.Latest) +
		mutedStyle.Render(" · run: ") +
		phaseStyle.Render(notice.Command)
}

func renderFooterRow(left, right string, width int) string {
	if width < 1 {
		width = 1
	}
	rw := tuiVisibleWidth(right)
	if rw > width {
		// the decorative right block never gets to widen the row
		right = trimToDisplayWidth(right, width)
		rw = tuiVisibleWidth(right)
	}
	maxLeft := width - rw - 1
	if maxLeft < 0 {
		maxLeft = 0
	}
	if lw := tuiVisibleWidth(left); lw > maxLeft {
		left = trimToDisplayWidth(left, maxLeft)
	}
	lw := tuiVisibleWidth(left)
	gap := width - lw - rw
	if gap < 1 {
		gap = 1
	}
	if lw+gap+rw > width {
		// tiny width: both trimmed blocks still can't share the row — drop
		// the left block and right-align rather than overflow the terminal.
		return strings.Repeat(" ", width-rw) + right
	}
	return left + strings.Repeat(" ", gap) + right
}

func renderSummaryFooter(width int, notice *selfupdate.Notice) []string {
	if notice == nil {
		return renderLiveScreenFooter(width)
	}
	if width < 1 {
		width = tuiDisplayWidth
	}
	rowW := width - leftPad
	if rowW < 1 {
		rowW = 1
	}
	right1 := renderFrostTagline(tuiFooterLine1, 0.0, 0.5)
	line1 := renderFooterRow(renderUpdateNoticeLine(notice), right1, rowW)
	line2 := renderFrostTaglineRight(tuiFooterLine2, rowW, 0.2, 0.7)
	return []string{"", line1, line2}
}

func renderLiveScreenFooter(width int) []string {
	if width < 1 {
		width = tuiDisplayWidth
	}
	right := width - leftPad
	if right < 1 {
		right = 1
	}
	return []string{
		"",
		renderFrostTaglineRight(tuiFooterLine1, right, 0.0, 0.5),
		renderFrostTaglineRight(tuiFooterLine2, right, 0.2, 0.7),
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func phaseVisuals(phase int32, m *search.Metrics) (pct float64, label string, boxStart, boxEnd, barStart, barEnd colorful.Color) {
	label = indexPhaseLabel(m)
	boxStart, boxEnd = frostGradA, frostGradB
	barStart, barEnd = frostGradA, frostGradB
	switch phase {
	case search.PhaseSearch:
		label = "SEARCHING"
		boxStart, boxEnd = gradStart, gradEnd
		barStart, barEnd = gradStart, gradEnd
		pct = searchPercent(m)
	case search.PhaseDone:
		label = "COMPLETE"
		boxStart, boxEnd = gradStart, gradEnd
		barStart, barEnd = gradStart, gradEnd
		pct = 1
		if pctSearch := searchPercent(m); pctSearch > pct {
			pct = pctSearch
		}
	default:
		pct = indexPercent(m)
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	return pct, label, boxStart, boxEnd, barStart, barEnd
}

func indexBytes(m *search.Metrics) (done, total int64) {
	done = m.IndexBytesDone.Load()
	total = m.IndexBytesTotal.Load()
	if total > 0 && done > total {
		done = total
	}
	return done, total
}

func searchBytes(m *search.Metrics) (done, total int64) {
	done = m.BytesScanned.Load()
	total = m.BytesScannedTotal.Load()
	if total > 0 && done > total {
		done = total
	}
	return done, total
}

// renderLibrarySizeRow shows the on-disk (compressed) library size — the figure
// that matches `du` — as the headline, with the uncompressed total and ratio
// trailing discreetly so it's clear compression is doing the heavy lifting.
// The ratio suffix is omitted when there's no meaningful compression (e.g. -txt).
func renderLibrarySizeRow(m *search.Metrics) string {
	compressed := m.IndexBytesTotal.Load()
	_, uncompressed := searchBytes(m)
	row := labelStyle.Render("Library   ") + byteStyle.Render(formatBytes(compressed)) + mutedStyle.Render(" on disk")
	if compressed > 0 && uncompressed > compressed {
		ratio := float64(uncompressed) / float64(compressed)
		ratioStr := fmt.Sprintf("%.0f", ratio)
		if ratio < 10 {
			ratioStr = fmt.Sprintf("%.1f", ratio)
		}
		row += mutedStyle.Render(fmt.Sprintf("  ·  %s uncompressed (%s×)", formatBytes(uncompressed), ratioStr))
	}
	return row
}

func searchScanPercent(m *search.Metrics) float64 {
	done, total := searchBytes(m)
	if total > 0 {
		return float64(done) / float64(total)
	}
	return 0
}

// two stacked bars w/ aligned percent suffix col.
// indexing: Index + Search(pending). searching: Chunks + Scanned
func renderLabeledProgressBars(phase int32, m *search.Metrics, barWidth int, start, end colorful.Color) [2]string {
	body := barWidth - progressBarLabelWidth
	if body < 8 {
		body = 8
	}
	switch phase {
	case search.PhaseSearch:
		return [2]string{
			indentStr + progressBarLabel("Chunks") + gradientBarWithPercent(searchPercent(m), body, start, end),
			indentStr + progressBarLabel("Scanned") + gradientBarWithPercent(searchScanPercent(m), body, start, end),
		}
	case search.PhaseDone:
		return [2]string{
			indentStr + progressBarLabel("Chunks") + gradientBarWithPercent(1, body, start, end),
			indentStr + progressBarLabel("Scanned") + gradientBarWithPercent(1, body, start, end),
		}
	default:
		return [2]string{
			indentStr + progressBarLabel("Index") + gradientBarWithPercent(indexPercent(m), body, frostGradA, frostGradB),
			indentStr + progressBarLabel("Search") + pendingBar(body),
		}
	}
}

func progressBarLabel(name string) string {
	s := labelStyle.Render(name)
	if w := lipgloss.Width(s); w < progressBarLabelWidth {
		return s + strings.Repeat(" ", progressBarLabelWidth-w)
	}
	return s
}

func pendingBar(width int) string {
	const suffixWidth = 7
	if width < suffixWidth+2 {
		width = suffixWidth + 2
	}
	body := width - suffixWidth
	const suffix = "   ----"
	return mutedStyle.Render(strings.Repeat("░", body) + suffix)
}

func searchPercent(m *search.Metrics) float64 {
	progress, total := searchChunkProgress(m)
	if total > 0 {
		return progress / float64(total)
	}
	return 0
}

func searchChunkProgress(m *search.Metrics) (float64, int64) {
	total := m.ChunksTotal.Load()
	if total <= 0 {
		return 0, 0
	}
	progress := float64(m.ChunksDone.Load())
	if scannedTotal := m.BytesScannedTotal.Load(); scannedTotal > 0 {
		scanned := m.BytesScanned.Load()
		if scanned > scannedTotal {
			scanned = scannedTotal
		}
		byteProgress := float64(scanned) / float64(scannedTotal) * float64(total)
		if byteProgress > progress {
			progress = byteProgress
		}
	}
	if progress > float64(total) {
		progress = float64(total)
	}
	return progress, total
}

func numDigits(n int64) int {
	if n < 0 {
		n = -n
	}
	digits := 1
	for n >= 10 {
		n /= 10
		digits++
	}
	return digits
}

func indexPercent(m *search.Metrics) float64 {
	done, total := indexBytes(m)
	var pct float64
	if total > 0 {
		pct = float64(done) / float64(total)
	}
	archDone := m.ArchivesIndexed.Load()
	archTotal := m.ArchivesTotal.Load()
	if archTotal > 0 {
		archPct := float64(archDone) / float64(archTotal)
		if archPct > pct {
			pct = archPct
		}
	}
	return pct
}

func indexPhaseLabel(m *search.Metrics) string {
	if m == nil {
		return "INDEXING"
	}
	if m.IndexFrameScanActive.Load() > 0 {
		return "INDEXING · frame scan"
	}
	if m.IndexDecodeActive.Load() > 0 {
		return "INDEXING · decode"
	}
	return "INDEXING"
}
