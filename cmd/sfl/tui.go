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
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
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
	fmt.Fprint(os.Stderr, ansiResetScroll+ansiShowCursor+altScreenLeave)
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
		b.WriteString(altScreenEnter + ansiHideCursor)
		f.altOn = true
	}
	// Clamp to one row shy of the terminal height so the worker panel can't
	// scroll the buffer on short terminals; Compose erases any rows a taller
	// previous frame (e.g. the extracting panel) left behind.
	b.WriteString(tuiframe.Compose(lines, termHeight()-1))
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
		frame.draw(renderProgress(now.Sub(started), prog, rate, spinnerTick(now), termWidth()))
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
	return frameWithFooter(header, box, width, footerLines(width))
}

func frameWithFooter(header string, box []string, width int, footer []string) []string {
	out := []string{"", header, ""}
	out = append(out, box...)
	return append(out, footer...)
}

func renderProgress(elapsed time.Duration, prog *sflog.Progress, rate float64, tick int, width int) []string {
	inner := boxInner(width)
	spinner := lineSpinnerFrames[mod(tick, len(lineSpinnerFrames))]

	// Ingest carries the same icy frame so the screen never hands off: a single
	// bar + "added / already-in-library" counts driven by the dedup engine.
	if prog.Phase() == phaseIngestVal {
		iv, _ := prog.IngestSnapshot()
		header := headerLine(sflSpinnerStyle.Render(spinner), sflOkStyle.Render("[sfl] INGESTING"), elapsed, width)
		bar := gradientBar(iv.Fraction, inner)
		counts := fmt.Sprintf("%s added  ·  %s already in library",
			formatInt(int(iv.Unique)), formatInt(int(iv.Skipped)))
		body := []string{bar, counts}
		if iv.Status != "" {
			body = append(body, sflMutedStyle.Render(iv.Status))
		}
		return frame(header, insetBox(sflBoxStyle, body, width), width)
	}

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
		// Surface the concurrent workers: a per-slot panel makes the
		// parallelism visible even when the byte bar crawls. Falls back to the
		// single current path when no registry is wired (back-compat).
		if panel := sflWorkerPanel(prog, inner, tick); len(panel) > 0 {
			body = append(body, panel...)
		} else if cur := prog.Current(); cur != "" {
			body = append(body, sflMutedStyle.Render(truncatePath(cur, inner)))
		}
	}

	return frame(header, insetBox(sflBoxStyle, body, width), width)
}

// sfl worker-panel sizing. A small floor keeps the "many things at once" feel
// even on short terminals; the panel expands toward the worker count when there
// is vertical room, mirroring sfu's OD frame.
const (
	sflMaxWorkerRows = 8
	// rows the extract frame needs for non-worker content (margins, header,
	// box borders, bar, counts, detail, footer), with margin for SIGWINCH.
	sflWorkerReservedRows = 12
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
	out = append(out, sflLabelStyle.Render(fmt.Sprintf("%d of %d workers active", len(active), total)))
	for _, w := range active {
		out = append(out, renderSflWorkerRow(w, inner, idxMarkerW, tick))
	}
	return out
}

// renderSflWorkerRow is one panel line: "[i] ⠹ <stage>  <path>". A braille
// spinner (phase-shifted by worker index for a gentle cascade) sits between the
// marker and the fixed-width stage column so paths still align; the path is
// truncated to fit.
func renderSflWorkerRow(w sflog.ActiveWorker, inner, idxMarkerW, tick int) string {
	marker := fmt.Sprintf("[%d]", w.Index+1)
	if pad := idxMarkerW - lipgloss.Width(marker); pad > 0 {
		marker += strings.Repeat(" ", pad)
	}
	stage := w.Stage.String()
	if stage == "" {
		stage = "working"
	}
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
		sflMutedStyle.Render(truncatePath(w.Path, pathW))
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
	phaseIngestVal   = 2 // mirrors sflog phaseIngest
	phaseDoneVal     = 3 // mirrors sflog phaseDone
)

// sflRecapLabelW is the label column width so recap values line up in a clean
// gutter (matches sfu's "Input "/"Unique " labels and fits "Ingested").
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
// frame can slot its "Ingested" row in before the issues.
func recapCountRows(stats sflog.ExtractStats) []string {
	return []string{
		recapRow("Logs", sflOkStyle.Render(formatInt(stats.Logs))+
			sflMutedStyle.Render("  ·  "+formatInt(stats.Credentials)+" parsed")),
		recapRow("Unique", sflOkStyle.Render(formatInt(stats.Emitted))+
			sflMutedStyle.Render("  ·  "+formatInt(stats.Duplicates)+" duplicates")),
		recapRow("Sources", sflMutedStyle.Render(fmt.Sprintf("%s files  ·  %s archives  ·  %s skipped",
			formatInt(stats.FilesScanned), formatInt(stats.ArchivesScanned), formatInt(stats.SkippedFiles+stats.SkippedArchives)))),
	}
}

// recapIssueBlock is the per-kind fail lines, set off by a leading blank when
// present so they breathe below the metric block. nil when there are none.
func recapIssueBlock(stats sflog.ExtractStats) []string {
	issues := issueLines(stats)
	if len(issues) == 0 {
		return nil
	}
	return append([]string{""}, issues...)
}

// summaryRecap is the shared extraction recap reused by the classic and
// no-ingest frames: the metric block followed by any issue lines.
func summaryRecap(stats sflog.ExtractStats) []string {
	return append(recapCountRows(stats), recapIssueBlock(stats)...)
}

func renderFinalSummary(outPath string, stats sflog.ExtractStats) []string {
	return renderFinalSummaryWithNotice(outPath, stats, nil)
}

func renderFinalSummaryWithNotice(outPath string, stats sflog.ExtractStats, notice *selfupdate.Notice) []string {
	width := termWidth()
	title := sflIndent + sflOkStyle.Render("✓ ") + sflTitleStyle.Render("SnowFastLog COMPLETE")
	body := append(summaryRecap(stats), "", sflMutedStyle.Render("Output: ")+outPath)
	return frameWithFooter(title, insetBox(sflBoxStyle, body, width), width, summaryFooterLines(width, notice))
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
	body := append(summaryRecap(stats),
		"",
		sflMutedStyle.Render("No credentials extracted — library unchanged."),
		sflMutedStyle.Render("Library: ")+libraryDir,
	)
	return frameWithFooter(title, insetBox(sflBoxStyle, body, width), width, summaryFooterLines(width, notice))
}

// renderIngestSummary is the -od completion frame: the same extraction recap,
// what this run contributed (new vs already-present), and the resulting library
// line count and path, so the single icy frame ends the run instead of handing
// off to sfu's summary.
func renderIngestSummary(libraryDir string, libraryLines, newToLib, alreadyInLib int64, stats sflog.ExtractStats) []string {
	return renderIngestSummaryWithNotice(libraryDir, libraryLines, newToLib, alreadyInLib, stats, nil)
}

func renderIngestSummaryWithNotice(libraryDir string, libraryLines, newToLib, alreadyInLib int64, stats sflog.ExtractStats, notice *selfupdate.Notice) []string {
	width := termWidth()
	title := sflIndent + sflOkStyle.Render("✓ ") + sflTitleStyle.Render("SnowFastLog INGESTED")
	// Ingested sits with the counts (before issues); Library path closes the box.
	body := append(recapCountRows(stats),
		recapRow("Ingested", sflOkStyle.Render(formatInt(int(newToLib)))+
			sflMutedStyle.Render(" new  ·  "+formatInt(int(alreadyInLib))+" already in library")),
	)
	body = append(body, recapIssueBlock(stats)...)
	body = append(body, "", sflMutedStyle.Render("Library: ")+libraryDir)
	// The running library total gets its own box below the recap (mirrors sfu's
	// renderODSummary) so the headline post-ingest number stands on its own.
	box := append(insetBox(sflBoxStyle, body, width), "")
	box = append(box, libraryTotalBox(libraryLines, width)...)
	return frameWithFooter(title, box, width, summaryFooterLines(width, notice))
}

// libraryTotalBox is the standalone "<N> lines in library" box, the single
// headline number after ingestion (prior library + new unique this run).
func libraryTotalBox(libraryLines int64, width int) []string {
	body := []string{
		sflOkStyle.Render(formatInt(int(libraryLines))),
		sflMutedStyle.Render("lines in library"),
	}
	return insetBox(sflBoxStyle, body, width)
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

// plural picks the singular or plural noun by count (1 → singular).
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// issueLines summarises non-fatal problems for the analyst: bad passwords,
// parse failures, and sources that yielded no ULP, with a few example paths.
// Each label is pluralised by count so a single fail never reads as "1 errors".
func issueLines(stats sflog.ExtractStats) []string {
	var lines []string
	add := func(n int, kind sflog.IssueKind, one, many string) {
		if n <= 0 {
			return
		}
		lines = append(lines, sflWarnStyle.Render(fmt.Sprintf("! %s %s", formatInt(n), plural(n, one, many)))+
			exampleSuffix(stats, kind, n))
	}
	add(stats.PasswordNotFound, sflog.IssuePasswordNotFound, "password not found", "passwords not found")
	add(stats.ParseErrors, sflog.IssueParseError, "parse error", "parse errors")
	add(stats.OpenErrors, sflog.IssueOpenError, "open error", "open errors")
	add(stats.NoULP, sflog.IssueNoULP, "source with no ULP", "sources with no ULP")
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
