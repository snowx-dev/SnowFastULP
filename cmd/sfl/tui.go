package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"golang.org/x/term"
)

const (
	ansiHideCursor = "\033[?25l"
	ansiShowCursor = "\033[?25h"
	altScreenEnter = "\033[?1049h"
	altScreenLeave = "\033[?1049l"

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
	sflTitleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#D4F1F9")).Bold(true)
	sflOkStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#8FE7FF")).Bold(true)
	sflMutedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8BA7B1"))
	sflWarnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#F2C14E"))
	sflLabelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#B8D8E0")).Bold(true)
	sflSpinnerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#8FE7FF")).Bold(true)
	sflBoxStyle     = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#5BA4C9")).
			Padding(0, 2)
	sflInterruptBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#E0B040")).
				Padding(0, 2)
	sflEmptyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#33464F"))

	// icy gradient: deep frost -> icy white, distinct from sfu's purple/pink
	iceStart, _ = colorful.Hex("#1E5F8C")
	iceEnd, _   = colorful.Hex("#CAF0F8")
)

// ASCII spinner, 4 frames at 100ms keyed off wall-clock (no animation tick).
var lineSpinnerFrames = []string{"|", "/", "-", "\\"}

func spinnerFrame(now time.Time) string {
	idx := (now.UnixMilli() / 100) % int64(len(lineSpinnerFrames))
	if idx < 0 {
		idx = 0
	}
	return lineSpinnerFrames[idx]
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

// frostTagline renders text along the icy gradient, faint, for the footer.
func frostTagline(text string, spanStart, spanEnd float64) string {
	run := []rune(text)
	if len(run) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range run {
		t := spanStart
		if len(run) > 1 {
			t = spanStart + (spanEnd-spanStart)*float64(i)/float64(len(run)-1)
		}
		c := iceStart.BlendLuv(iceEnd, t)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Faint(true).Render(string(r)))
	}
	return b.String()
}

// frostTaglineRight right-aligns a frost tagline within width cells so its
// right edge lines up with the box's right edge.
func frostTaglineRight(text string, width int, spanStart, spanEnd float64) string {
	styled := frostTagline(text, spanStart, spanEnd)
	if width <= 0 {
		return ""
	}
	if vw := lipgloss.Width(styled); vw < width {
		return strings.Repeat(" ", width-vw) + styled
	}
	return styled
}

// footerLines is the blank + two right-aligned frost taglines drawn below the
// box, matching sfs's live/summary footer.
func footerLines(width int) []string {
	right := width - sflLeftPad
	if right < 1 {
		right = 1
	}
	return []string{
		"",
		frostTaglineRight(sflFooterLine1, right, 0.0, 0.5),
		frostTaglineRight(sflFooterLine2, right, 0.2, 0.7),
	}
}

func stdoutIsCharDevice() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w <= 0 {
		return sflDisplayWidth
	}
	if w > sflDisplayWidth {
		return sflDisplayWidth
	}
	return w
}

// gradientBar renders an icy progress bar with a right-aligned percent suffix.
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
		c := iceStart.BlendLuv(iceEnd, t)
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render("█"))
	}
	if rem := body - fill; rem > 0 {
		b.WriteString(sflEmptyStyle.Render(strings.Repeat("░", rem)))
	}
	b.WriteString(sflMutedStyle.Render(fmt.Sprintf(" %5.1f%%", percent*100)))
	return b.String()
}

// stderrFrame is a fixed alt-screen block redrawn in place each tick. Falls
// back to nothing on a non-TTY so piped runs stay clean.
type stderrFrame struct {
	tty   bool
	altOn bool
}

func (f *stderrFrame) close() {
	if !f.tty || !f.altOn {
		return
	}
	fmt.Fprint(os.Stderr, ansiResetScroll+ansiShowCursor+altScreenLeave)
	f.altOn = false
}

func (f *stderrFrame) draw(lines []string) {
	if !f.tty || len(lines) == 0 {
		return
	}
	if !f.altOn {
		fmt.Fprint(os.Stderr, altScreenEnter+ansiHideCursor)
		f.altOn = true
	}
	fmt.Fprint(os.Stderr, "\033[H")
	for _, ln := range lines {
		fmt.Fprint(os.Stderr, "\033[2K\r", ln, "\n")
	}
}

// monitor samples Progress every ~200ms and redraws the live frame until done.
// It registers the frame's restore hook so a force-exit (second Ctrl-C) or
// fatal error still leaves the alt-screen and shows the cursor.
func monitor(done <-chan struct{}, started time.Time, prog *sflog.Progress, signaled func() bool, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}
	frame := stderrFrame{tty: true}
	setTerminalRestore(frame.close)
	defer clearTerminalRestore()
	defer frame.close()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var prevAt time.Time
	var prevBytes int64
	var rate float64

	draw := func() {
		now := time.Now()
		if signaled != nil && signaled() {
			frame.draw(renderInterrupt(now.Sub(started), spinnerFrame(now), termWidth()))
			return
		}
		cur := prog.DoneBytes()
		if !prevAt.IsZero() {
			if dt := now.Sub(prevAt).Seconds(); dt >= 0.05 {
				rate = float64(cur-prevBytes) / dt
			}
		}
		prevAt, prevBytes = now, cur
		frame.draw(renderProgress(now.Sub(started), prog, rate, spinnerFrame(now), termWidth()))
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
// frame sits balanced in the terminal instead of flush-left.
func insetBox(style lipgloss.Style, body []string, width int) []string {
	rendered := style.Width(boxInner(width) + 4).Render(strings.Join(body, "\n"))
	pad := strings.Repeat(" ", sflLeftPad)
	lines := strings.Split(rendered, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return lines
}

// frame is the shared shape for every render: a blank top margin, the header /
// title row, a blank separator, the boxed body, then the footer.
func frame(header string, box []string, width int) []string {
	out := []string{"", header, ""}
	out = append(out, box...)
	return append(out, footerLines(width)...)
}

func renderProgress(elapsed time.Duration, prog *sflog.Progress, rate float64, spinner string, width int) []string {
	inner := boxInner(width)
	scanning := prog.Phase() == phaseDiscoverVal || prog.Total() == 0
	phase := "EXTRACTING"
	if prog.Phase() == phaseDoneVal {
		phase = "COMPLETE"
	} else if scanning {
		phase = "SCANNING"
	}

	header := headerLine(sflSpinnerStyle.Render(spinner), sflOkStyle.Render("[sfl] "+phase), elapsed, width)

	var body []string
	if scanning {
		// During discovery the total weight is unknown, so show a live "found"
		// count instead of a frozen 0% bar.
		body = []string{
			sflMutedStyle.Render(fmt.Sprintf("discovering sources… %s found", formatInt(int(prog.Discovered())))),
		}
	} else {
		bar := gradientBar(prog.Fraction(), inner)
		counts := fmt.Sprintf("%s / %s logs  ·  %s unique  ·  %s dupes",
			formatInt(int(prog.Logs())), formatInt(int(prog.LogsTotal())),
			formatInt(int(prog.Emitted())), formatInt(int(prog.Duplicates())))
		detail := sflMutedStyle.Render(fmt.Sprintf("%s files · %s archives · %s / %s · %s/s",
			formatInt(int(prog.Files())), formatInt(int(prog.Archives())),
			formatBytes(prog.DoneBytes()), formatBytes(prog.Total()), formatBytes(int64(rate))))
		body = []string{bar, counts, detail}
		if cur := prog.Current(); cur != "" {
			body = append(body, sflMutedStyle.Render(truncatePath(cur, inner)))
		}
	}

	return frame(header, insetBox(sflBoxStyle, body, width), width)
}

// renderInterrupt is the frame shown after a graceful Ctrl-C while in-flight
// reads finish and partial output is discarded.
func renderInterrupt(elapsed time.Duration, spinner string, width int) []string {
	header := headerLine(sflWarnStyle.Render(spinner), sflWarnStyle.Render("[!] INTERRUPTED — cleaning up"), elapsed, width)
	body := []string{
		"Finishing in-flight reads and discarding partial output.",
		"",
		sflMutedStyle.Render("A second Ctrl+C will force-exit immediately."),
	}
	return frame(header, insetBox(sflInterruptBoxStyle, body, width), width)
}

const (
	phaseDiscoverVal = 0 // mirrors sflog phaseDiscover
	phaseDoneVal     = 3 // mirrors sflog phaseDone
)

func renderFinalSummary(outPath string, stats sflog.ExtractStats) []string {
	width := termWidth()
	title := sflIndent + sflOkStyle.Render("✓ ") + sflTitleStyle.Render("SnowFastLog COMPLETE")
	body := []string{
		fmt.Sprintf("%s logs  ·  %s parsed  ·  %s unique  ·  %s duplicates",
			formatInt(stats.Logs), formatInt(stats.Credentials), formatInt(stats.Emitted), formatInt(stats.Duplicates)),
		fmt.Sprintf("%s files scanned  ·  %s archives  ·  %s skipped",
			formatInt(stats.FilesScanned), formatInt(stats.ArchivesScanned), formatInt(stats.SkippedFiles+stats.SkippedArchives)),
	}
	body = append(body, issueLines(stats)...)
	body = append(body, sflMutedStyle.Render("Output: ")+outPath)

	return frame(title, insetBox(sflBoxStyle, body, width), width)
}

// renderInterruptSummary is printed on the normal screen after a graceful
// Ctrl-C, replacing a bare "interrupted" line with a styled notice so the exit
// reads as deliberate rather than a crash.
func renderInterruptSummary(elapsed time.Duration) []string {
	width := termWidth()
	title := sflIndent + sflWarnStyle.Render("⚠ SnowFastLog INTERRUPTED")
	body := []string{
		"Stopped before completion — partial output discarded.",
		sflMutedStyle.Render(fmt.Sprintf("Ran for %s · re-run to start over.", formatDuration(elapsed))),
	}
	return frame(title, insetBox(sflInterruptBoxStyle, body, width), width)
}

// issueLines summarises non-fatal problems for the analyst: bad passwords,
// parse failures, and sources that yielded no ULP, with a few example paths.
func issueLines(stats sflog.ExtractStats) []string {
	var lines []string
	if stats.PasswordNotFound > 0 {
		lines = append(lines, sflWarnStyle.Render(fmt.Sprintf("! %s password not found", formatInt(stats.PasswordNotFound)))+
			exampleSuffix(stats, sflog.IssuePasswordNotFound, stats.PasswordNotFound))
	}
	if stats.ParseErrors > 0 {
		lines = append(lines, sflWarnStyle.Render(fmt.Sprintf("! %s parse errors", formatInt(stats.ParseErrors)))+
			exampleSuffix(stats, sflog.IssueParseError, stats.ParseErrors))
	}
	if stats.NoULP > 0 {
		lines = append(lines, sflWarnStyle.Render(fmt.Sprintf("! %s sources with no ULP", formatInt(stats.NoULP)))+
			exampleSuffix(stats, sflog.IssueNoULP, stats.NoULP))
	}
	return lines
}

// exampleSuffix lists a couple of example paths for an issue kind. total is the
// true count (not the capped example count) so "+N more" is accurate.
func exampleSuffix(stats sflog.ExtractStats, kind sflog.IssueKind, total int) string {
	const maxShown = 2
	var shown []string
	for _, is := range stats.Issues {
		if is.Kind != kind {
			continue
		}
		if len(shown) < maxShown {
			shown = append(shown, baseName(is.Path))
		}
	}
	if len(shown) == 0 {
		return ""
	}
	suffix := ": " + strings.Join(shown, ", ")
	if total > len(shown) {
		suffix += fmt.Sprintf(" (+%s more)", formatInt(total-len(shown)))
	}
	return sflMutedStyle.Render(suffix)
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func truncatePath(p string, max int) string {
	if max < 8 {
		max = 8
	}
	if len(p) <= max {
		return p
	}
	return "…" + p[len(p)-(max-1):]
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
