package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/secrets"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/termctl"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
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
	sflTitleStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFDDE6")).Bold(true)
	sflOkStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8FA6")).Bold(true)
	sflUniqueStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})
	sflAcceptStyle       = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})
	sflCountStyle        = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "33", Dark: "51"})
	sflByteStyle         = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "178", Dark: "222"})
	sflMutedStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6818F"))
	sflWarnStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("#F2C14E"))
	sflLabelStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#E8B6C6")).Bold(true)
	sflSpinnerStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8FA6")).Bold(true)
	sflInterruptBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#E0B040")).
				Padding(0, 2)
	sflEmptyStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#3A242E"))

	// sflDoneFill is the calm sage-green for a completed track's solid bar
	// (mirrors sfu's solidGreenFill): "done, move on" without dominating the
	// frame the way the live red gradient would.
	sflDoneFill = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "65", Dark: "71"})

	// elegant red gradient: muted plum-raspberry -> heart-emoji red (Twemoji ❤️
	// #DD2E44). Heart red is the main color; the start is a light, close plum so
	// it reads as a hint of purple without a long color span. Distinct from
	// sfu/sfs's light indigo->magenta and the amber interrupt accent.
	gradStart, _ = colorful.Hex("#9E3A6E")
	gradEnd, _   = colorful.Hex("#DD2E44")

	// footer taglines, ice blue → icy white (unified with sfu/sfs footers)
	footerGradA, _ = colorful.Hex("#7DD3E8")
	footerGradB, _ = colorful.Hex("#F2F8FC")

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

// frostTagline renders text along the ice-blue footer gradient, faint, for the
// footer. The open-source heart keeps its own bright red.
func frostTagline(text string, spanStart, spanEnd float64) string {
	run := []rune(text)
	if len(run) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range run {
		if r == '❤' || r == '\uFE0F' {
			b.WriteString(lipgloss.NewStyle().Foreground(heartRed).Render(string(r)))
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
	return left + strings.Repeat(" ", gap) + right
}

// stderrIsTTY reports whether the live frame's target (stderr) is a terminal.
// The TUI and the summary both render to stderr, so this — not stdout — is what
// gates colored output and the alt-screen frame (mirrors sfs).
func stderrIsTTY() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

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
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w <= 0 {
		return sflDisplayWidth
	}
	return w
}

// tuiVisibleWidth counts a string's printable columns, skipping ANSI SGR
// escapes so styled lines measure by what the terminal actually shows.
func tuiVisibleWidth(s string) int {
	b := []byte(s)
	i, n := 0, 0
	for i < len(b) {
		if b[i] == '\033' && i+1 < len(b) && b[i+1] == '[' {
			i += 2
			for i < len(b) && b[i] != 'm' {
				i++
			}
			if i < len(b) {
				i++
			}
			continue
		}
		_, sz := utf8.DecodeRune(b[i:])
		if sz == 0 {
			i++
			continue
		}
		i += sz
		n++
	}
	return n
}

// trimToDisplayWidth clamps a (possibly ANSI-styled) line to max printable
// columns, preserving escape sequences and appending an ellipsis. Frame rows are
// run through this before tuiframe.Compose so a line never exceeds the terminal
// width and soft-wraps (which would desync Compose's per-row cursor math and
// reintroduce ghosting on narrow terminals).
func trimToDisplayWidth(s string, max int) string {
	if max < 1 {
		max = 1
	}
	if tuiVisibleWidth(s) <= max {
		return s
	}
	var b strings.Builder
	v := 0
	bytes := []byte(s)
	i := 0
	for i < len(bytes) {
		if bytes[i] == '\033' && i+1 < len(bytes) && bytes[i+1] == '[' {
			j := i + 2
			for j < len(bytes) && bytes[j] != 'm' {
				j++
			}
			if j < len(bytes) {
				b.Write(bytes[i : j+1])
				i = j + 1
			} else {
				i = j
			}
			continue
		}
		if v >= max-1 {
			break
		}
		r, sz := utf8.DecodeRune(bytes[i:])
		if sz == 0 {
			i++
			continue
		}
		b.Write(bytes[i : i+sz])
		i += sz
		if r != utf8.RuneError {
			v++
		}
	}
	b.WriteString("\033[0m")
	b.WriteString("…")
	return b.String()
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
		b.WriteString(sflEmptyStyle.Render(strings.Repeat("░", rem)))
	}
	b.WriteString(sflMutedStyle.Render(fmt.Sprintf(" %5.1f%%", percent*100)))
	return b.String()
}

// sflPendingBar is the indeterminate Secrets bar shown while a streaming source
// (rar, encrypted-header 7z, nested) is open: the candidate denominator is not
// final, so any percentage would lie (scanned/total ≈ 1 because total chases
// scanned member-by-member). It renders an all-empty trough with a muted "----"
// suffix in place of a number, matching gradientBar's cell width so the two
// bars stay aligned. The row's "Y+" signal still conveys that the denominator
// is growing.
func sflPendingBar(width int) string {
	if width < barSuffixWidth+2 {
		width = barSuffixWidth + 2
	}
	body := width - barSuffixWidth
	return sflEmptyStyle.Render(strings.Repeat("░", body)) + sflMutedStyle.Render("   ----")
}

// stderrFrame is a fixed alt-screen block redrawn in place each tick. Falls
// back to nothing on a non-TTY so piped runs stay clean. Draw and close are
// serialized so a force-exit (second Ctrl+C / cleanup timeout) can never
// interleave between line writes and spill the frame onto the primary screen.
// close is idempotent.
type stderrFrame struct {
	mu    sync.Mutex
	tty   bool
	altOn bool
}

func (f *stderrFrame) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	fmt.Fprint(os.Stderr, b.String())
}

// monitor samples Progress every ~200ms and redraws the live frame until done.
// It registers the frame's restore hook so a force-exit (second Ctrl-C) or
// fatal error still leaves the alt-screen and shows the cursor.
func monitor(done <-chan struct{}, started time.Time, prog *sflog.Progress, signaled func() bool, wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}
	frame := stderrFrame{tty: stderrIsTTY()}
	reg.Set(frame.close)
	defer reg.Clear()
	defer frame.close()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var prevAt time.Time
	var prevBytes int64
	var byteRate float64    // instant throughput for the Bytes row
	var scanFracMax float64 // monotonic clamp for the secret-scan bar (never regress)

	draw := func() {
		now := time.Now()
		if signaled != nil && signaled() {
			frame.draw(renderInterrupt(now.Sub(started), spinnerFrame(now), termWidth(), ulpengine.SnapshotCleanupLog()))
			return
		}
		cur := prog.DoneBytes()
		if !prevAt.IsZero() {
			if dt := now.Sub(prevAt).Seconds(); dt >= 0.05 {
				byteRate = float64(cur-prevBytes) / dt
			}
		}
		prevAt, prevBytes = now, cur
		// Freeze the monotonic clamp while a streaming source is open: its
		// scanned/total ≈ 1 (total chases scanned) would pin scanFracMax at 100%,
		// hiding the real scan tail that follows once extraction finishes. Resume
		// tracking only when the denominator is final.
		if prog.SecretStreamsOpen() == 0 {
			if sf := prog.ScanFraction(); sf > scanFracMax {
				scanFracMax = sf
			}
		}
		frame.draw(renderProgress(now.Sub(started), prog, byteRate, scanFracMax, spinnerTick(now), termWidth()))
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

// renderProgress draws one live frame. scanFrac is the secondary secret-scan
// bar's value, passed in (not read from prog) so the monitor can clamp it
// monotonically: the raw scanned/total ratio can dip when a streaming/encrypted
// archive that could not be pre-counted inflates the total at open, and the
// user must never see the bar move backwards.
func renderProgress(elapsed time.Duration, prog *sflog.Progress, byteRate, scanFrac float64, tick int, width int) []string {
	inner := boxInner(width)
	spinner := lineSpinnerFrames[mod(tick, len(lineSpinnerFrames))]

	// Ingest carries the same icy frame so the screen never hands off: labeled
	// stats rows (mirroring extract) plus optional regen worker rows.
	if prog.Phase() == phaseIngestVal {
		iv, _ := prog.IngestSnapshot()
		header := headerLine(sflSpinnerStyle.Render(spinner), sflOkStyle.Render("[sfl] INGESTING"), elapsed, width)
		bar := gradientBar(iv.Fraction, inner)
		body := append([]string{bar}, renderIngestStatsRows(iv)...)
		if iv.Status != "" {
			body = append(body, sflMutedStyle.Render(iv.Status))
		}
		if panel := renderIngestRegenPanel(iv.Workers, inner, tick); len(panel) > 0 {
			body = append(body, panel...)
		}
		return frame(header, sflGradientBox(body, width, gradStart, gradEnd), width)
	}

	// Dedicated secrets-flush phase: extraction is done, the store is draining.
	// A moving spinner + the running found count so the hand-off to the summary
	// never reads as a frozen 100%.
	if prog.Phase() == phaseSecretsFinalizeVal {
		header := headerLine(sflSpinnerStyle.Render(spinner), sflOkStyle.Render("[sfl] FINALIZING SECRETS"), elapsed, width)
		box := sflGradientBox([]string{
			recapRow("Secrets", sflUniqueStyle.Render(formatInt(int(prog.SecretsFound())))+sflMutedStyle.Render(" found")),
			sflMutedStyle.Render("writing to store…"),
		}, width, gradStart, gradEnd)
		// Extraction and scanning are both done here (solid green); the moving
		// spinner in the header carries the "still working" signal while the
		// store drains, mirroring sfu's completed-phase bars.
		bw := sflBarBody(width)
		bars := []string{
			sflIndent + sflBarLabel("Extract") + sflSolidBar(1.0, bw, sflDoneFill),
			sflIndent + sflBarLabel("Secrets") + sflSolidBar(1.0, bw, sflDoneFill),
		}
		return sflFrameWithBars(header, box, bars, nil, width)
	}

	scanning := prog.Phase() == phaseDiscoverVal || prog.Total() == 0
	phase := "EXTRACTING"
	if prog.Phase() == phaseDoneVal {
		phase = "COMPLETE"
	} else if scanning {
		phase = "SCANNING"
	} else if prog.SecretsEnabled() && prog.Fraction() >= 1 && scanFrac < 1 {
		// Bytes are fully read but the CPU-bound secret scan is still draining;
		// label the tail honestly instead of a misleading "EXTRACTING" at 100%.
		phase = "SCANNING SECRETS"
	}

	header := headerLine(sflSpinnerStyle.Render(spinner), sflOkStyle.Render("[sfl] "+phase), elapsed, width)

	var body []string
	if scanning {
		// During discovery the total weight is unknown, so show a live "found"
		// count instead of a frozen 0% bar.
		body = []string{
			sflMutedStyle.Render("discovering sources… ") +
				sflCountStyle.Render(formatInt(int(prog.Discovered()))) +
				sflMutedStyle.Render(" found"),
		}
	} else {
		statRows := renderExtractStatsRows(
			prog.Files(), prog.Archives(),
			prog.Logs(), prog.LogsTotal(), prog.Emitted(), prog.Duplicates(),
			prog.DoneBytes(), prog.Total(), byteRate,
		)
		if prog.SecretsEnabled() {
			// -secrets frame, sfu-style: a stats box (with the live found/scanned
			// row), the two labeled bars below it, then the worker panel in its
			// own box under the bars. Both tracks are active gradients since
			// extraction and secret scanning run in the same pass.
			boxBody := append(statRows, renderSecretsLiveRow(prog.SecretsFound(), prog.SecretFilesScanned(), prog.SecretFilesTotal(), prog.SecretStreamsOpen() > 0))
			box := sflGradientBox(boxBody, width, gradStart, gradEnd)
			// The Secrets bar is indeterminate while any streaming source is
			// open: its denominator is still growing, so a percentage would lie.
			bars := renderSflBarPair("Extract", prog.Fraction(), "Secrets", scanFrac, prog.SecretStreamsOpen() > 0, width)
			return sflFrameWithBars(header, box, bars, sflWorkerPanelBox(prog, width, inner, tick), width)
		}
		body = append([]string{gradientBar(prog.Fraction(), inner)}, statRows...)
		body = appendSflWorkerBlock(body, prog, inner, tick)
	}

	return frame(header, sflGradientBox(body, width, gradStart, gradEnd), width)
}

// appendSflWorkerBlock appends the concurrent-worker panel (or the single
// current-path fallback when no registry is wired) to a frame's box body.
func appendSflWorkerBlock(body []string, prog *sflog.Progress, inner, tick int) []string {
	if panel := sflWorkerPanel(prog, inner, tick); len(panel) > 0 {
		return append(body, panel...)
	}
	if cur := prog.Current(); cur != "" {
		return append(body, sflMutedStyle.Render(truncatePath(cur, inner)))
	}
	return body
}

// sflBarLabelW is the fixed label column for the paired -secrets bars so the
// two percent suffixes line up under each other (mirrors sfu's progressBarLabel).
const sflBarLabelW = 9 // "Extract  " / "Secrets  "

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
	empty := sflEmptyStyle.Render(strings.Repeat("░", body-fill))
	return full + empty + sflMutedStyle.Render(fmt.Sprintf(" %5.1f%%", percent*100))
}

// renderSflBarPair builds the two labeled bar rows placed below the stats box on
// a -secrets run, mirroring sfu's parsing/dedup pair: an indented label column
// then the bar spanning the box width. Both tracks are active gradients here
// (extraction bytes + secret-scan coverage run concurrently, like sfu's fast
// path); the finalize frame swaps the extract bar for a solid completed one.
func renderSflBarPair(l1 string, p1 float64, l2 string, p2 float64, p2pending bool, width int) []string {
	body := sflBarBody(width)
	second := gradientBar(p2, body)
	if p2pending {
		// Secrets denominator is not final (a streaming source is open): show
		// an indeterminate trough instead of a misleading percentage.
		second = sflPendingBar(body)
	}
	return []string{
		sflIndent + sflBarLabel(l1) + gradientBar(p1, body),
		sflIndent + sflBarLabel(l2) + second,
	}
}

// sflFrameWithBars lays out a -secrets frame the way sfu stacks its progress
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

// sflSecretsFrameOverhead is every non-worker-row line in the two-bar -secrets
// frame: top margin + header + blank (3), the stats box (2 borders + 5 stat
// rows + the Secrets row = 8), the two bar rows each with a blank separator (4),
// the worker box's blank separator + 2 borders + "N workers" header (4), and the
// footer (3). The worker panel is padded to a fixed height that fits under this
// so the footer keeps a constant screen row instead of being pushed off (and
// redrawn) as the busy-worker count changes — the -secrets footer-flicker fix.
const sflSecretsFrameOverhead = 22

// sflSecretsWorkerRows is the fixed worker-row count for the -secrets frame: as
// many as fit beneath the frame overhead, capped at sflMaxWorkerRows and the
// worker count, so the total frame height stays constant tick-to-tick. Returns 0
// when the terminal is too short for even one row, so the panel is dropped and
// the footer still shows.
func sflSecretsWorkerRows(termH, totalWorkers int) int {
	if totalWorkers <= 0 {
		return 0
	}
	rows := termH - 1 - sflSecretsFrameOverhead
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

// sflWorkerPanelBox renders the concurrent-worker panel as its own gradient box
// below the -secrets bars (mirroring how sfu stacks its worker frame under the
// progress bars). It is drawn at a FIXED height — a reserved header row plus a
// constant number of worker rows padded with blanks — so the box, and the footer
// beneath it, never move as workers start and finish. Returns nil only when the
// terminal is too short to fit any worker row.
func sflWorkerPanelBox(prog *sflog.Progress, width, inner, tick int) []string {
	rows := sflSecretsWorkerRows(termHeight(), prog.WorkerCount())
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

// renderSecretsLiveRow is the live "Secrets" counter row: how many secrets have
// been found, and how many files have been scanned out of the candidates known
// so far (X / Y, Y growing as archives open) so the user sees what is left.
// streaming is true while any non-pre-counted source (rar, encrypted-header 7z,
// nested) is still open; in that state Y is not final, so a "+" is appended
// ("Y+") to signal the denominator is still growing instead of presenting a
// misleadingly complete total.
func renderSecretsLiveRow(found, scanned, total int64, streaming bool) string {
	scannedTxt := sflCountStyle.Render(formatInt(int(scanned)))
	if total > 0 {
		scannedTxt += sflMutedStyle.Render(" / " + formatInt(int(total)))
		if streaming {
			scannedTxt += sflMutedStyle.Render("+")
		}
	}
	return recapRow("Secrets", sflUniqueStyle.Render(formatInt(int(found)))+
		sflMutedStyle.Render(" found  ·  ")+scannedTxt+
		sflMutedStyle.Render(" files scanned"))
}

// renderExtractStatsRows is the labeled live-stats block during extraction.
// One metric group per row mirrors recapCountRows so large counts never compete
// for width inside the box. ETA was removed: archive bursts and the secret-scan
// tail made remaining/rate too inconsistent to be honest, so the row is gone
// along with its per-tick EMA computation.
func renderExtractStatsRows(files, archives, logs, logsTotal, emitted, dupes, doneBytes, totalBytes int64, byteRate float64) []string {
	return []string{
		recapRow("Logs", sflCountStyle.Render(formatInt(int(logs)))+
			sflMutedStyle.Render(" / "+formatInt(int(logsTotal)))),
		recapRow("Unique", sflUniqueStyle.Render(formatInt(int(emitted)))+
			sflMutedStyle.Render("  ·  ")+sflCountStyle.Render(formatInt(int(dupes)))+
			sflMutedStyle.Render(" dupes")),
		recapRow("Sources", sflCountStyle.Render(formatInt(int(files)))+
			sflMutedStyle.Render(" files  ·  ")+sflCountStyle.Render(formatInt(int(archives)))+
			sflMutedStyle.Render(" archives")),
		recapRow("Bytes", sflByteStyle.Render(formatBytes(doneBytes))+
			sflMutedStyle.Render(" / "+formatBytes(totalBytes)+"  ·  ")+
			sflByteStyle.Render(formatBytes(int64(byteRate))+"/s")),
	}
}

// renderIngestStatsRows is the labeled live-stats block during library ingest.
// Rows appear only when relevant so regen/shard phases never show a frozen 0/0.
func renderIngestStatsRows(iv sflog.IngestView) []string {
	var rows []string
	if ingestShowLibraryRow(iv) {
		rows = append(rows, recapRow("Library", renderIngestLibraryValue(iv)))
	}
	if ingestShowULPRow(iv) {
		rows = append(rows, recapRow("ULP", sflByteStyle.Render(formatBytes(iv.BytesRead))+
			sflMutedStyle.Render(" / "+formatBytes(iv.ULPBytes)+"  ·  ")+
			sflAcceptStyle.Render(formatInt(int(iv.LinesRead)))+
			sflMutedStyle.Render(" lines")))
	}
	if iv.ShowMerge {
		merge := sflUniqueStyle.Render(formatInt(int(iv.Unique))) + sflMutedStyle.Render(" added") +
			sflMutedStyle.Render("  ·  ") + sflCountStyle.Render(formatInt(int(iv.Skipped))) +
			sflMutedStyle.Render(" already in library")
		if iv.BucketsTotal > 0 {
			merge += sflMutedStyle.Render("  ·  bucket ") +
				sflCountStyle.Render(formatInt(int(iv.BucketsDone))) +
				sflMutedStyle.Render(" / ") +
				sflCountStyle.Render(formatInt(int(iv.BucketsTotal)))
		}
		rows = append(rows, recapRow("Merge", merge))
	}
	return rows
}

func ingestShowLibraryRow(iv sflog.IngestView) bool {
	if iv.PartsRegenTotal > 0 || iv.RegenBytesTotal > 0 {
		return true
	}
	if iv.ArchivesTotal > 0 && iv.EnginePhase != ulpengine.PhaseDedup && iv.EnginePhase != ulpengine.PhaseDone {
		return true
	}
	return false
}

func ingestShowULPRow(iv sflog.IngestView) bool {
	if iv.ULPBytes <= 0 {
		return false
	}
	if iv.BytesRead < iv.ULPBytes || iv.LinesRead > 0 {
		return true
	}
	switch iv.EnginePhase {
	case ulpengine.PhaseInit, ulpengine.PhasePhase0, ulpengine.PhaseShard:
		return true
	}
	return false
}

func renderIngestLibraryValue(iv sflog.IngestView) string {
	var parts []string
	if iv.PartsRegenTotal > 0 {
		parts = append(parts, sflCountStyle.Render(formatInt(int(iv.PartsRegenDone)))+
			sflMutedStyle.Render(" / "+formatInt(int(iv.PartsRegenTotal))+" parts"))
	}
	if iv.RegenBytesTotal > 0 {
		parts = append(parts, sflByteStyle.Render(formatBytes(iv.RegenBytesRead))+
			sflMutedStyle.Render(" / "+formatBytes(iv.RegenBytesTotal)))
	}
	if iv.RegenBPS > 0 {
		parts = append(parts, sflMutedStyle.Render("·  ")+
			sflByteStyle.Render(formatBytes(int64(iv.RegenBPS))+"/s"))
	}
	if len(parts) == 0 && iv.ArchivesTotal > 0 {
		parts = append(parts, sflCountStyle.Render(formatInt(int(iv.ArchivesTotal)))+
			sflMutedStyle.Render(" archives"))
	}
	return strings.Join(parts, " ")
}

const (
	sflIngestReservedRows = 18
	sflIngestMaxRegenRows = 8
)

// sflIngestRegenRowCap limits regen worker rows shown during ingest.
func sflIngestRegenRowCap(termHeight, totalWorkers int) int {
	if totalWorkers <= 0 {
		return 0
	}
	available := termHeight - sflIngestReservedRows
	if available < sflIngestMaxRegenRows {
		available = sflIngestMaxRegenRows
	}
	if available > totalWorkers {
		available = totalWorkers
	}
	return available
}

// renderIngestRegenPanel shows per-archive regen/index worker activity during ingest.
func renderIngestRegenPanel(workers []sflog.IngestWorker, inner, tick int) []string {
	if len(workers) == 0 {
		return nil
	}
	out := make([]string, 0, len(workers)+1)
	if len(workers) >= 2 {
		out = append(out, sflLabelStyle.Render(fmt.Sprintf("%d workers active", len(workers))))
	}
	for i, w := range workers {
		out = append(out, renderIngestRegenRow(w, inner, tick, i))
	}
	return out
}

func renderIngestRegenRow(w sflog.IngestWorker, inner, tick, idx int) string {
	name := compactIngestArchiveName(w.Archive)
	partAnnot := ""
	if w.PartsTotal > 1 {
		partAnnot = fmt.Sprintf(" (%d/%d)", w.PartIdx, w.PartsTotal)
	}
	var pct float64
	if w.BytesTotal > 0 {
		pct = float64(w.BytesDone) / float64(w.BytesTotal)
		if pct > 1 {
			pct = 1
		}
	}
	barW := 12
	if barW+40 > inner {
		barW = inner - 40
		if barW < 6 {
			barW = 6
		}
	}
	bar := gradientBar(pct, barW)
	var pctText string
	if w.BytesTotal > 0 {
		pctText = fmt.Sprintf("%3d%%", int(pct*100))
	} else {
		pctText = "  ?%"
	}
	bytesText := ""
	if w.BytesTotal > 0 {
		bytesText = formatBytes(w.BytesDone) + " / " + formatBytes(w.BytesTotal)
	}
	// spinner + name + part + bar + pct + bytes
	nameW := inner - barW - lipgloss.Width(partAnnot) - lipgloss.Width(pctText) - 12
	if nameW < 8 {
		nameW = 8
	}
	line := sflSpinnerStyle.Render(workerSpinnerFrame(tick, idx)) + " " +
		sflMutedStyle.Render(truncatePath(name, nameW)+partAnnot) + "  " +
		bar + " " + sflCountStyle.Render(pctText)
	if bytesText != "" {
		line += "  " + sflByteStyle.Render(bytesText)
	}
	return line
}

func compactIngestArchiveName(path string) string {
	base := filepath.Base(path)
	if strings.HasPrefix(base, "sfu_") && strings.HasSuffix(base, ".txt.zst") {
		base = strings.TrimSuffix(strings.TrimPrefix(base, "sfu_"), ".txt.zst")
	}
	return base
}

// sfl worker-panel sizing. A small floor keeps the "many things at once" feel
// even on short terminals; the panel expands toward the worker count when there
// is vertical room, mirroring sfu's OD frame.
const (
	sflMaxWorkerRows = 8
	// rows the extract frame needs for non-worker content (margins, header,
	// box borders, bar, five labeled stats rows, footer), with margin for SIGWINCH.
	sflWorkerReservedRows = 15
	// widest stage label ("testing password"); the stage column is padded to
	// this so paths line up across rows.
	sflStageColW = 16
)

// sflWorkerRowCap is the per-worker row budget for termHeight and totalWorkers.
// Pure so tests can pass arbitrary heights. Never more than totalWorkers, never
// fewer than sflMaxWorkerRows, expanding toward totalWorkers on tall terminals.
func sflWorkerRowCap(termHeight, totalWorkers int) int {
	if totalWorkers <= 0 {
		return 0
	}
	available := termHeight - sflWorkerReservedRows
	if available < sflMaxWorkerRows {
		available = sflMaxWorkerRows
	}
	if available > totalWorkers {
		available = totalWorkers
	}
	return available
}

// sflWorkerPanel reads the live worker registry and renders it. It returns nil
// when no registry is wired so the caller can fall back to the single
// current-path line. tick drives the per-row spinner animation.
func sflWorkerPanel(prog *sflog.Progress, inner, tick int) []string {
	total := prog.WorkerCount()
	if total <= 0 {
		return nil
	}
	active := prog.ActiveWorkers(sflWorkerRowCap(termHeight(), total))
	return renderSflWorkerPanel(active, total, inner, tick)
}

// renderSflWorkerPanel is the pure renderer: a header count plus one row per
// busy worker slot. Empty active set renders nothing.
func renderSflWorkerPanel(active []sflog.ActiveWorker, total, inner, tick int) []string {
	if len(active) == 0 {
		return nil
	}
	idxMarkerW := lipgloss.Width(fmt.Sprintf("[%d]", total))
	out := make([]string, 0, len(active)+1)
	// Only call out the worker count when 2+ are genuinely busy: a single active
	// row plus a "1 workers active" header makes a (correctly) one-stream archive
	// look like wasted cores, so collapse to just the row in that case.
	if len(active) >= 2 {
		out = append(out, sflLabelStyle.Render(fmt.Sprintf("%d workers active", len(active))))
	}
	for _, w := range active {
		out = append(out, renderSflWorkerRow(w, inner, idxMarkerW, tick))
	}
	return out
}

// sflBothRecentWindow is how recently a slot must have done both ULP and secret
// work to be labeled "ulp + secrets" — wide enough to survive the ~200ms redraw
// cadence and the per-member alternation of a sequential archive reader, narrow
// enough that a worker that has moved on to only one activity reverts to its
// single-activity label within a couple seconds.
const sflBothRecentWindow = 2 * time.Second

// sflWorkerStageLabel renders the panel label for one worker, collapsing to
// "ulp + secrets" when the slot has both pulled ULPs and scanned secrets within
// sflBothRecentWindow — the "this archive is doing both right now" case on
// sequential RAR/7z streams where credential and secret members interleave.
// Otherwise it falls back to the per-stage label. Width fits sflStageColW
// ("ulp + secrets" is 13 cells, under the 16-cell column).
func sflWorkerStageLabel(w sflog.ActiveWorker) string {
	now := time.Now()
	if !w.LastULP.IsZero() && !w.LastSec.IsZero() &&
		now.Sub(w.LastULP) <= sflBothRecentWindow && now.Sub(w.LastSec) <= sflBothRecentWindow {
		return "ulp + secrets"
	}
	return sflStageLabel(w.Stage)
}

// sflStageLabel maps a worker stage to its user-facing panel label. It widens
// the short canonical names from WorkerStage.String() so the panel reads as
// intent rather than a bare verb: the credential/secret actions name what they
// operate on ("extracting ulps", "parsing ulps", "scanning secrets"), while the
// archive-prep actions stay short ("opening", "testing password"). Every label
// fits the fixed sflStageColW (16) column — "scanning secrets" and "testing
// password" are the widest at exactly 16 — so paths still align without a width
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
	case sflog.StageScanning:
		return "scanning secrets"
	default:
		return "working"
	}
}

// renderSflWorkerRow is one panel line: "[i] ⠹ <stage>  <path>". A braille
// spinner (phase-shifted by worker index for a gentle cascade) sits between the
// marker and the fixed-width stage column so paths still align; the path is
// truncated to fit. The stage label is the explicit TUI form, collapsed to
// "ulp + secrets" via sflWorkerStageLabel when the slot is doing both.
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
		sflMutedStyle.Render(truncatePath(workerPathLabel(w.Path), pathW))
}

// termHeight is the terminal row count (stderr), defaulting to 24 when unknown
// so the worker panel still sizes sensibly on a non-TTY/redirected run.
func termHeight() int {
	_, h, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// renderInterrupt is the frame shown after a graceful Ctrl-C while in-flight
// reads finish and partial output is discarded.
func renderInterrupt(elapsed time.Duration, spinner string, width int, cleanupLog []string) []string {
	header := headerLine(sflWarnStyle.Render(spinner), sflWarnStyle.Render("[!] INTERRUPTED — cleaning up"), elapsed, width)
	body := []string{
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
	phaseDiscoverVal        = 0 // mirrors sflog phaseDiscover
	phaseIngestVal          = 2 // mirrors sflog phaseIngest
	phaseDoneVal            = 3 // mirrors sflog phaseDone
	phaseSecretsFinalizeVal = 4 // mirrors sflog phaseSecretsFinalize
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

// recapCountRows is the labeled metric block (one row per group so big counts
// never wrap mid-number). Kept separate from the issue block so the ingest
// frame can slot library-ingest rows before the issues block.
func recapCountRows(stats sflog.ExtractStats) []string {
	return []string{
		recapRow("Logs", sflOkStyle.Render(formatInt(stats.Logs))+
			sflMutedStyle.Render("  ·  "+formatInt(stats.Credentials)+" parsed")),
		recapRow("Unique", sflUniqueStyle.Render(formatInt(stats.Emitted))+
			sflMutedStyle.Render("  ·  "+formatInt(stats.Duplicates)+" duplicates")),
		recapRow("Sources", sflMutedStyle.Render(fmt.Sprintf("%s files  ·  %s archives  ·  %s skipped",
			formatInt(stats.FilesScanned), formatInt(stats.ArchivesScanned), formatInt(stats.SkippedFiles+stats.SkippedArchives)))),
	}
}

// renderSflRemovedRows mirrors sfu's Removed recap: one line when it fits, else
// stacked bullets under the label.
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
	if totalWidth <= maxInnerWidth {
		return []string{sflLabelStyle.Render(label) + singleLineRest}
	}
	indent := strings.Repeat(" ", lipgloss.Width(label))
	rows := []string{sflLabelStyle.Render(label) + bullets[0]}
	for _, b := range bullets[1:] {
		rows = append(rows, indent+b)
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
func renderIngestLibraryRows(newToLib, alreadyInLib, dropped int64, innerWidth int) []string {
	rows := []string{recapRow("Added", sflUniqueStyle.Render(formatInt(int(newToLib)))+
		sflMutedStyle.Render(" entries"))}
	var bullets []string
	if dropped > 0 {
		bullets = append(bullets, sflWarnStyle.Render(formatInt(int(dropped)))+" "+sflMutedStyle.Render("rejected"))
	}
	if alreadyInLib > 0 {
		bullets = append(bullets, sflCountStyle.Render(formatInt(int(alreadyInLib)))+" "+sflMutedStyle.Render("already in library"))
	}
	if len(bullets) > 0 {
		rows = append(rows, renderSflRemovedRows(bullets, innerWidth)...)
	}
	return rows
}

func renderFinalSummary(outPath string, stats sflog.ExtractStats) []string {
	return renderFinalSummaryWithNotice(outPath, stats, nil)
}

func renderFinalSummaryWithNotice(outPath string, stats sflog.ExtractStats, notice *selfupdate.Notice) []string {
	width := termWidth()
	title := sflIndent + sflOkStyle.Render("✓ ") + sflTitleStyle.Render("SnowFastLog COMPLETE")
	body := append(recapCountRows(stats), "", sflMutedStyle.Render("Output: ")+outPath)
	box := sflGradientBox(body, width, gradStart, gradEnd)
	return frameWithFooter(title, box, width, summaryFooterLines(width, notice))
}

// renderNoIngestSummary is the -od frame when extraction produced no
// credentials: a calm "done, nothing to do" recap with the library left
// untouched, rather than an error exit.
func renderNoIngestSummary(libraryDir string, stats sflog.ExtractStats) []string {
	return renderNoIngestSummaryWithNotice(libraryDir, stats, nil)
}

func renderNoIngestSummaryWithNotice(libraryDir string, stats sflog.ExtractStats, notice *selfupdate.Notice) []string {
	width := termWidth()
	title := sflIndent + sflOkStyle.Render("✓ ") + sflTitleStyle.Render("SnowFastLog COMPLETE")
	body := append(recapCountRows(stats),
		"",
		sflMutedStyle.Render("No credentials extracted — library unchanged."),
		sflMutedStyle.Render("Library: ")+libraryDir,
	)
	box := sflGradientBox(body, width, gradStart, gradEnd)
	return frameWithFooter(title, box, width, summaryFooterLines(width, notice))
}

// renderIngestSummary is the -od completion frame: the same extraction recap,
// what this run contributed (new vs already-present), and the resulting library
// line count and path, so the single icy frame ends the run instead of handing
// off to sfu's summary.
func renderIngestSummary(libraryDir string, libraryLines, newToLib, alreadyInLib, dropped int64, stats sflog.ExtractStats, outputPaths []string) []string {
	return renderIngestSummaryWithNotice(libraryDir, libraryLines, newToLib, alreadyInLib, dropped, stats, outputPaths, nil)
}

func renderIngestSummaryWithNotice(libraryDir string, libraryLines, newToLib, alreadyInLib, dropped int64, stats sflog.ExtractStats, outputPaths []string, notice *selfupdate.Notice) []string {
	width := termWidth()
	title := sflIndent + sflOkStyle.Render("✓ ") + sflTitleStyle.Render("SnowFastLog INGESTED")
	// Box holds clean stats only: extraction recap, Added/Removed ingest rows,
	// library path. Failures/skips are streamed to the -err file (never stdout);
	// the running library total gets its own box below (mirrors sfu's renderODSummary).
	body := append(recapCountRows(stats), renderIngestLibraryRows(newToLib, alreadyInLib, dropped, boxInner(width))...)
	body = append(body, "", sflMutedStyle.Render("Library: ")+libraryDir)
	box := sflGradientBox(body, width, gradStart, gradEnd)
	box = append(box, "")
	box = append(box, libraryTotalBox(libraryLines, width)...)
	out := []string{"", title, ""}
	out = append(out, box...)
	out = append(out, renderIngestOutputFooter(outputPaths)...)
	out = append(out, summaryFooterLines(width, notice)...)
	return out
}

// renderIngestOutputFooter lists archive(s) written this run below the ingest
// summary boxes, mirroring sfu's renderDoneOutputFooter layout.
func renderIngestOutputFooter(paths []string) []string {
	const label = "Output   "
	mid := gradStart.BlendLuv(gradEnd, 0.5)
	border := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	labelCell := sflLabelStyle.Render(label)
	labelW := lipgloss.Width(labelCell)
	prefix := strings.Repeat(" ", sflLeftPad) + border.Render("┃") + "  "
	blankLabel := strings.Repeat(" ", labelW)

	// Empty after a completed ingest = nothing new was added (all duplicates); the
	// engine discarded the empty shard, so state that plainly instead of dropping
	// the row (which would leave the user wondering where the output went).
	if len(paths) == 0 {
		return []string{"", prefix + labelCell + sflMutedStyle.Render("(nothing new)")}
	}

	out := []string{""}
	for i, p := range paths {
		pathCell := sflOkStyle.Render(p)
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

// renderSecretsBlock is the standalone secrets recap appended to the summary
// when -secrets is on: a blank spacer then a peer gradient box reporting what
// this run found (new vs. already stored) and where the store lives. Styled like
// the other recap boxes so it reads as a sibling of the credential summary.
func renderSecretsBlock(stats secrets.Stats, dbPath string, width int) []string {
	value := sflUniqueStyle.Render(formatInt(int(stats.New))) + sflMutedStyle.Render(" new") +
		sflMutedStyle.Render("  ·  ") + sflCountStyle.Render(formatInt(int(stats.Existing))) +
		sflMutedStyle.Render(" already stored")
	if stats.DupInRun > 0 {
		value += sflMutedStyle.Render("  ·  ") + sflCountStyle.Render(formatInt(int(stats.DupInRun))) +
			sflMutedStyle.Render(" dupes")
	}
	body := []string{
		recapRow("Secrets", value),
	}
	if stats.Deduped > 0 {
		body = append(body, recapRow("Skipped", sflCountStyle.Render(formatInt(int(stats.Deduped)))+
			sflMutedStyle.Render(" duplicate files (already scanned)")))
	}
	body = append(body, recapRow("Store", sflMutedStyle.Render(dbPath)))
	return append([]string{""}, sflGradientBox(body, width, gradStart, gradEnd)...)
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

// clampHead trims s to at most max runes, keeping the start (the file name) and
// marking the cut with an ellipsis, so a pathological member name can't
// soft-wrap the muted block across the terminal.
func clampHead(s string, max int) string {
	if max < 1 {
		max = 1
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
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

func truncatePath(p string, max int) string {
	if max < 8 {
		max = 8
	}
	// Count and slice on rune boundaries so a UTF-8 path (or the "▸" nested
	// separator) is never cut mid-rune into mojibake.
	r := []rune(p)
	if len(r) <= max {
		return p
	}
	return "…" + string(r[len(r)-(max-1):])
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
