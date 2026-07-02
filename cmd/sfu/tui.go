package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/selfupdate"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
	"golang.org/x/term"
)

// 80-col live status block. alt-screen + cursor-home redraw per tick,
// falls back to plain stdout when not a TTY

const (
	// 86 leaves room for the bordered stat block (4-col indent + 82 box)
	// w/o cramming rows. nothing important hidden at exactly 80
	tuiDisplayWidth = 86
	leftPad         = 4
	indentSpace     = "    "

	// right-aligned tagline at bottom of live TUI and DONE block.
	// frost-blue → icy-white gradient continues across both lines
	tuiFooterLine1 = "sfu is open-source ❤️"
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

const (
	ansiHideCursor = "\033[?25l"
	ansiShowCursor = "\033[?25h"
	altScreenEnter = "\033[?1049h"
	altScreenLeave = "\033[?1049l"
	// kept as bare SGR-reset, trimToDisplayWidth needs to emit one
	// before the ellipsis when truncating mid-styled string. lipgloss
	// cant help b/c trim runs after lipgloss already rendered the line
	ansiReset = "\033[0m"
)

// palette. semantic roles, lipgloss+termenv degrade on NO_COLOR/dumb/non-TTY

var (
	// phase tags ([1/2 PARSING], COMPLETE, ...)
	phaseStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "33", Dark: "51"})

	// rotating bar. bright magenta, no clash w/ cyan phase tag
	spinnerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "162", Dark: "213"})

	// section labels (Throughput / Lines / Progress / System)
	labelStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})

	// separators (·), inline noise words, "----" placeholder
	mutedStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})

	// elapsed clock, top-right of every header
	timeStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "162", Dark: "213"})

	// raw byte counts + B/s rates. amber reads as "data flowing",
	// stays distinct from timeStyle pink
	byteStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "178", Dark: "222"})

	// integer counts (chunks, buckets, workers)
	countStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "33", Dark: "51"})

	// accepted line count, parse phase
	acceptStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})

	// unique line count, dedup phase + DONE summary
	uniqueStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})

	// rejected count, reads "investigate me"
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})

	// system metrics. RAM violet, off green axis (avoids accept/unique
	// collision) and off cyan axis (separate from CPU)
	cpuStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "33", Dark: "51"})
	ramStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "99", Dark: "141"})

	// ✓ on DONE line
	okStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})

	// unfilled ░ portion of bars
	emptyStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "250", Dark: "238"})

	// completed phase-1 bar during phase 2. calmer sage so 80 cells of
	// solid fill dont dominate, reads "done, move on"
	solidGreenFill = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "65", Dark: "71"})
)

// gradient endpoints. two ramps:
//   - shard/dedup: purple→pink, same as bubbletea WithDefaultGradient
//   - done: forest→mint green, keeps completion in green family
//
// per-char LUV blend via go-colorful for perceptually uniform ramp
var (
	gradStart, _ = colorful.Hex("#5A56E0") // purple
	gradEnd, _   = colorful.Hex("#EE6FF8") // pink

	doneStart, _ = colorful.Hex("#3CC451") // vivid medium green
	doneEnd, _   = colorful.Hex("#88FF7B") // bright lime, ~xterm 82

	// live footer taglines, ice blue → icy white
	footerGradA, _ = colorful.Hex("#7DD3E8")
	footerGradB, _ = colorful.Hex("#F2F8FC")

	// box frames / mini progress bars, frost blue → icy white
	frostGradA, _ = colorful.Hex("#3D7EA6")
	frostGradB, _ = colorful.Hex("#F2F8FC")

	// open-source heart in the footer, bright red ❤️
	heartRed = lipgloss.Color("#FF2B2B")

	// interrupt frame, amber → muted red
	interruptStart, _ = colorful.Hex("#E0B040")
	interruptEnd, _   = colorful.Hex("#C04030")
)

// spinner. ASCII-only, 4 frames at 100ms = 400ms rotation
var lineSpinnerFrames = []string{"|", "/", "-", "\\"}

// rotating-bar frame keyed off wall-clock ms (sampled at draw, no anim tick)
func spinnerFrame(now time.Time) string {
	idx := (now.UnixMilli() / 100) % int64(len(lineSpinnerFrames))
	if idx < 0 {
		idx = 0
	}
	return lineSpinnerFrames[idx]
}

// live terminal width, capped at tuiDisplayWidth. polled per tick so
// SIGWINCH resizes show up within ~300ms
func termWidth() int {
	w := termWidthFull()
	if w > tuiDisplayWidth {
		return tuiDisplayWidth
	}
	return w
}

// termWidthFull is the real terminal width, uncapped. The muted cleanup log
// above the interrupt box uses this so long paths truncate as late as possible.
func termWidthFull() int {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w <= 0 {
		return tuiDisplayWidth
	}
	return w
}

// terminal row count. VT100 24-row default on non-TTY / query failure.
// used by OD frame to budget per-worker rows
func termHeight() int {
	_, h, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// progress bars. gradientBar (active), solidBar (completed), pendingBar
// (queued). shared barSuffixWidth so percent labels align across rows

const barSuffixWidth = 7 // " 100.0%"

// fixed title column for parsing/dedup bars, aligns percent suffixes
const progressBarLabelWidth = 9 // "Deduping "

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
		// stretch gradient across full bar (not just filled) so
		// visible colours shift slowly as percent grows. matches
		// bubbletea progress.WithDefaultGradient (non-scaled)
		t := 0.0
		if body > 1 {
			t = float64(i) / float64(body-1)
		}
		c := gradStart.BlendLuv(gradEnd, t)
		b.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color(c.Hex())).
			Render("█"))
	}
	if rem := body - fill; rem > 0 {
		b.WriteString(emptyStyle.Render(strings.Repeat("░", rem)))
	}
	suffix := fmt.Sprintf(" %5.1f%%", percent*100)
	b.WriteString(mutedStyle.Render(suffix))
	return b.String()
}

func solidBar(percent float64, width int, fillStyle lipgloss.Style) string {
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
	empty := emptyStyle.Render(strings.Repeat("░", body-fill))
	suffix := fmt.Sprintf(" %5.1f%%", percent*100)
	return full + empty + mutedStyle.Render(suffix)
}

// "queued" placeholder bar, all empty + dashed suffix. exactly
// barSuffixWidth chars in the suffix to align w/ other bars
func pendingBar(width int) string {
	if width < barSuffixWidth+2 {
		width = barSuffixWidth + 2
	}
	body := width - barSuffixWidth
	const suffix = "   ----" // 7 chars, matches " 100.0%"
	return mutedStyle.Render(strings.Repeat("░", body) + suffix)
}

func progressBarLabel(name string) string {
	s := labelStyle.Render(name)
	if w := lipgloss.Width(s); w < progressBarLabelWidth {
		return s + strings.Repeat(" ", progressBarLabelWidth-w)
	}
	return s
}

func mainPhaseBarWidth(width int) int {
	body := contentWidth(width) - progressBarLabelWidth
	min := barSuffixWidth + 2
	if body < min {
		return min
	}
	return body
}

// parsing/dedup bar pair below the stat frame.
// parsing phase: active Parsing + queued Deduping.
// dedup phase: solid Parsing + active Deduping
func renderMainProgressBars(parsingPct, dedupPct float64, parsingComplete bool, width int) [2]string {
	body := mainPhaseBarWidth(width)
	if parsingComplete {
		return [2]string{
			indentSpace + progressBarLabel("Parsing") + solidBar(1.0, body, solidGreenFill),
			indentSpace + progressBarLabel("Deduping") + gradientBar(dedupPct, body),
		}
	}
	return [2]string{
		indentSpace + progressBarLabel("Parsing") + gradientBar(parsingPct, body),
		indentSpace + progressBarLabel("Deduping") + pendingBar(body),
	}
}

func renderFastPathProgressBars(pct float64, width int) [2]string {
	body := mainPhaseBarWidth(width)
	return [2]string{
		indentSpace + progressBarLabel("Parsing") + gradientBar(pct, body),
		indentSpace + progressBarLabel("Deduping") + gradientBar(pct, body),
	}
}

// width-aware utilities. visible-width math skips SGR escapes so
// lipgloss-styled strings measure correctly

func tuiVisibleWidth(s string) int {
	b := []byte(s)
	i := 0
	n := 0
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

// stderrIsCharDevice gates the live TUI: the alt-screen and per-tick frames
// render to stderr (keeping stdout clean for `sfu -o ./out | grep`), so the
// gate must follow stderr's TTY status, not stdout's.
func stderrIsCharDevice() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// applyStderrColorProfile downgrades lipgloss to plain ASCII when stderr is not
// a terminal. Both the live TUI and the DONE summary render to stderr, so when
// stderr is redirected (e.g. `sfu ... 2> run.log`, even with stdout a TTY) the
// log must stay free of ANSI escapes. Mirrors sfl/sfs.
func applyStderrColorProfile() {
	if !stderrIsCharDevice() {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

// fixed multi-line status block on the alt-screen. each draw homes the cursor,
// rewrites every line, then erases below to wipe a taller prior frame. Draw and
// close are serialized so a teardown from the signal/force-exit goroutine can
// never interleave between line writes and spill onto the primary screen. close
// is idempotent.
type tuiFrame struct {
	mu    sync.Mutex
	tty   bool
	altOn bool
}

func (f *tuiFrame) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.tty || !f.altOn {
		return
	}
	// reset any scroll region defensively before leaving the alt-screen.
	fmt.Fprint(os.Stderr, "\033[r"+ansiShowCursor+altScreenLeave)
	f.altOn = false
}

// wipes viewport on SIGWINCH so next draw lays out from a clean state
func (f *tuiFrame) redrawOnResize() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.tty || !f.altOn {
		return
	}
	fmt.Fprint(os.Stderr, "\033[2J\033[H")
}

func (f *tuiFrame) draw(lines []string) {
	if !f.tty || len(lines) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]string, len(lines))
	for i, ln := range lines {
		rows[i] = trimToDisplayWidth(ln, tuiDisplayWidth)
	}
	var b strings.Builder
	if !f.altOn {
		b.WriteString(altScreenEnter + ansiHideCursor)
		f.altOn = true
	}
	// Clamp to one row shy of the terminal height so the bottom line can't
	// scroll the buffer; Compose erases any stale rows below.
	b.WriteString(tuiframe.Compose(rows, termHeight()-1))
	fmt.Fprint(os.Stderr, b.String())
}

func trimToDisplayWidth(s string, max int) string {
	if tuiVisibleWidth(s) <= max {
		return s
	}
	var b strings.Builder
	v := 0
	i := 0
	bytes := []byte(s)
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
	b.WriteString(ansiReset)
	b.WriteString("…")
	return b.String()
}

// number / duration formatting. counts get thousands separators
// (no K/M/B shorthand) for at-a-glance comparability

func formatDuration(d time.Duration) string {
	total := int64(d.Seconds())
	h := total / 3600
	mm := (total % 3600) / 60
	ss := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, mm, ss)
}

func formatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	sign := ""
	if s[0] == '-' {
		sign = "-"
		s = s[1:]
	}
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	var b strings.Builder
	b.WriteString(sign)
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// compact K/M/B/T for header badges and library rows at billion scale.
// sub-million values keep formatCount commas for precision.
func formatCompactCount(n int64) string {
	if n < 0 {
		return "0"
	}
	if n < compactCountThreshold {
		return formatCount(n)
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

const compactCountThreshold = 1_000_000

func formatLibraryCount(n int64) string {
	if n >= compactCountThreshold {
		return formatCompactCount(n)
	}
	return formatCount(n)
}

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
	// roll to next unit at 1000 (not 1024) so display never exceeds
	// 8 chars. divisor stays 1024, only the threshold moves earlier
	// so 1000 MB displays as "1.0 GB" instead of "1000.0 MB"
	for v >= 1000 && u < len(units)-1 {
		v /= 1024
		u++
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}

func formatRate(bps float64) string {
	if bps <= 0 {
		return "0 B/s"
	}
	if bps < 1024 {
		return fmt.Sprintf("%.0f B/s", bps)
	}
	return humanBytes(int64(bps)) + "/s"
}

// layout helpers

// labeled pipeline step count. -od = 3 (parse, dedup, output index),
// else 2 (parse, dedup)
func tuiPhaseTotal(r *ulpengine.Resolved) int {
	if r != nil && r.Cfg.DestDedup {
		return 3
	}
	return 2
}

// 1-based step label, eg "[2/3 DEDUPING]"
func renderPhaseTag(r *ulpengine.Resolved, step int, label string) string {
	return renderPhaseTagWithTotal(tuiPhaseTotal(r), step, label)
}

// step-1 tag while -od library prep runs. parsing still active → PARSING;
// inputs fully read but sidecar work continues → LIBRARY PREP.
func renderStep1PhaseTag(r *ulpengine.Resolved, m *ulpengine.Metrics) string {
	if r != nil && r.Cfg.DestDedup && ulpengine.ODPhaseInFlight(r.OdMetrics) && shardInputsFullyRead(m, r) {
		return renderPhaseTag(r, 1, "LIBRARY PREP")
	}
	return renderPhaseTag(r, 1, "PARSING")
}

// muted header badges after the phase tag during dedup.
func renderDedupHeaderBadges(r *ulpengine.Resolved) string {
	if r == nil {
		return ""
	}
	var badges []string
	if r.Cfg.DestDedup && r.OdMetrics != nil {
		if total := r.OdMetrics.KeysTotalEstimate.Load(); total > 0 {
			badges = append(badges, "vs "+formatLibraryCount(total)+" library")
		}
	}
	if r.Cfg.Compress {
		badges = append(badges, "compressing")
	}
	if len(badges) == 0 {
		return ""
	}
	var out strings.Builder
	for _, b := range badges {
		out.WriteString(" ")
		out.WriteString(mutedStyle.Render("· " + b))
	}
	return out.String()
}

func shardInputsFullyRead(m *ulpengine.Metrics, r *ulpengine.Resolved) bool {
	if m == nil || r == nil {
		return false
	}
	if ct := m.ChunksTotal.Load(); ct > 0 && m.ChunksDone.Load() >= ct {
		return true
	}
	if r.TotalInputs > 0 && m.BytesRead.Load() >= r.TotalInputs {
		return true
	}
	return false
}

// for -od-only panels where total is always 3 even if test omits DestDedup
func renderPhaseTagWithTotal(total, step int, label string) string {
	return fmt.Sprintf("[%d/%d %s]", step, total, label)
}

// indented spinner + phase tag, elapsed pushed flush right
func renderHeader(icon, phase string, elapsed time.Duration, width int) string {
	left := indentSpace + icon + "  " + phaseStyle.Render(phase)
	right := timeStyle.Render(formatDuration(elapsed))
	pad := (width - leftPad) - tuiVisibleWidth(left) - tuiVisibleWidth(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// prepends left pad and clamps to width
func indentLine(s string, width int) string {
	return trimToDisplayWidth(indentSpace+s, width)
}

// prepends n spaces to every line. used to inset DONE box
func indentBlock(s string, n int) string {
	if n <= 0 {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// "<n> rejected" in muted gray. shared mutedStyle on number AND word
// reads as one de-emphasised unit, leaving accept/unique to draw the eye
func renderRejected(n int64) string {
	return mutedStyle.Render(formatCount(n) + " rejected")
}

// first-stat column in boxed rows. "Throughput"+3, "Lines"+8,
// "Progress"+5, "System"+7 all sum to 13
const statLabelColWidth = 13

// renders name in labelStyle, right-pads to exactly statLabelColWidth
func statLabel(name string) string {
	s := labelStyle.Render(name)
	vw := tuiVisibleWidth(s)
	if vw < statLabelColWidth {
		return s + strings.Repeat(" ", statLabelColWidth-vw)
	}
	return s
}

// one row of the stacked Lines layout
type lineStat struct {
	sublabel string
	value    string
	style    lipgloss.Style
}

// returns rows for Lines metric, inline if it fits else 3-row stacked.
// stacks only when inline would be ellipsised, so typical runs keep
// compact look. on stack we ignore inline and rebuild from stats
func renderLinesRow(inline string, stats []lineStat, innerW int) []string {
	if innerW <= 0 || tuiVisibleWidth(inline) <= innerW {
		return []string{inline}
	}
	maxValW := 0
	maxSubW := 0
	for _, s := range stats {
		if w := tuiVisibleWidth(s.value); w > maxValW {
			maxValW = w
		}
		if w := tuiVisibleWidth(s.sublabel); w > maxSubW {
			maxSubW = w
		}
	}
	header := statLabel("Lines")
	blank := strings.Repeat(" ", statLabelColWidth)

	out := make([]string, 0, len(stats))
	for i, s := range stats {
		prefix := blank
		if i == 0 {
			prefix = header
		}
		sub := mutedStyle.Render(padRight(s.sublabel, maxSubW))
		val := s.style.Render(padLeft(s.value, maxValW))
		out = append(out, prefix+sub+"  "+val)
	}
	return out
}

// pads plain ASCII s w/ trailing spaces to visible width w
func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// padRight mirror, prepends spaces (right-align s within w)
func padLeft(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

// fixed widths for jiggly high-frequency rows.
//
//	rateColWidth: "999.9 GB/s" (10) + 1 slack
//	bytesColWidth: "999.9 GB" (8)
const (
	rateColWidth  = 11
	bytesColWidth = 8
)

// pads/trims styled s to exactly w visible cells. used by gradientBox
// to keep rows flush against the right border
func padOrTrim(s string, w int) string {
	if w <= 0 {
		return ""
	}
	vw := tuiVisibleWidth(s)
	if vw == w {
		return s
	}
	if vw < w {
		return s + strings.Repeat(" ", w-vw)
	}
	return trimToDisplayWidth(s, w)
}

// bordered box w/ per-char LUV gradient on top/bottom borders, solid
// mid-tone on verticals. inner content padded/truncated to fit.
// outerWidth = full row width (corner + body + corner), inner = outer-6
func gradientBox(innerLines []string, outerWidth int, start, end colorful.Color) string {
	const minWidth = 8
	if outerWidth < minWidth {
		outerWidth = minWidth
	}
	inner := outerWidth - 6
	if inner < 1 {
		inner = 1
	}

	mid := start.BlendLuv(end, 0.5)
	midStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))

	// border w/ single LUV-blended ramp across all outerWidth cells,
	// corners carry the endpoints
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
			b.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color(c.Hex())).
				Render(ch))
		}
		return b.String()
	}

	rows := make([]string, 0, len(innerLines)+2)
	rows = append(rows, buildBorder("╭", "╮"))
	for _, ln := range innerLines {
		row := midStyle.Render("│") + "  " + padOrTrim(ln, inner) + "  " + midStyle.Render("│")
		rows = append(rows, row)
	}
	rows = append(rows, buildBorder("╰", "╯"))
	return strings.Join(rows, "\n")
}

// "Removed N rej · M dup · K already in library" recap row. single
// line when it fits, else one bullet per row indented under the label.
// nil when bullets empty
func renderRemovedRows(bullets []string, maxInnerWidth int) []string {
	if len(bullets) == 0 {
		return nil
	}
	const label = "Removed  " // 9 cells, matches Input/Output/Unique
	sep := mutedStyle.Render(" · ")

	// try single-line first. tuiVisibleWidth strips ANSI styling
	singleLineRest := strings.Join(bullets, sep)
	totalWidth := tuiVisibleWidth(label) + tuiVisibleWidth(singleLineRest)
	if totalWidth <= maxInnerWidth {
		return []string{labelStyle.Render(label) + singleLineRest}
	}

	// multi-line fallback, one bullet per row. continuation rows
	// indented to stack vertically under the bullet column
	indent := strings.Repeat(" ", tuiVisibleWidth(label))
	rows := make([]string, 0, len(bullets))
	rows = append(rows, labelStyle.Render(label)+bullets[0])
	for _, b := range bullets[1:] {
		rows = append(rows, indent+b)
	}
	return rows
}

// live -od dedup row: full comma-separated key counts, single line when
// innerW allows else label on row 1 and counts on row 2 (no ellipsis).
func renderLibraryMatchingRows(done, total int64, innerW int) []string {
	const label = "Library      " // 13 cells, matches Progress/System rows
	doneStr := countStyle.Render(formatCount(done))
	totalStr := countStyle.Render(formatCount(total))
	countsPart := doneStr + mutedStyle.Render(" / ") + totalStr + mutedStyle.Render(" loaded")
	singleLineRest := mutedStyle.Render("matching · ") + countsPart
	labelRendered := labelStyle.Render(label)
	totalWidth := tuiVisibleWidth(label) + tuiVisibleWidth(singleLineRest)
	if innerW <= 0 || totalWidth <= innerW {
		return []string{labelRendered + singleLineRest}
	}
	indent := strings.Repeat(" ", tuiVisibleWidth(label))
	return []string{
		labelRendered + mutedStyle.Render("matching"),
		indent + countsPart,
	}
}

// chars fmt would use for n in base 10
func numDigits(n int64) int {
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

// per-rune frost gradient without width padding.
func renderFrostTagline(text string, spanStart, spanEnd float64) string {
	run := []rune(text)
	if len(run) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range run {
		// the open-source heart keeps its own bright red, not the frost ramp
		if r == '❤' || r == '\uFE0F' {
			b.WriteString(lipgloss.NewStyle().Foreground(heartRed).Render(string(r)))
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

// per-rune frost gradient, left-padded to width cells, right-aligned.
// Faint for discreet footer
func renderFrostTaglineRight(text string, width int, spanStart, spanEnd float64) string {
	styled := renderFrostTagline(text, spanStart, spanEnd)
	if width <= 0 {
		if width < 0 {
			width = 0
		}
		return strings.Repeat(" ", width)
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
	return warnStyle.Render("Update available: v"+notice.Latest) +
		mutedStyle.Render(" · run: ") +
		phaseStyle.Render(notice.Command)
}

func renderFooterRow(left, right string, width int) string {
	if width < 1 {
		width = 1
	}
	rw := tuiVisibleWidth(right)
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
	right1 := renderFrostTagline(tuiFooterLine1, 0.0, 0.5)
	line1 := renderFooterRow(renderUpdateNoticeLine(notice), right1, rowW)
	line2 := renderFrostTaglineRight(tuiFooterLine2, rowW, 0.2, 0.7)
	return []string{"", line1, line2}
}

// blank + two right-aligned taglines for bottom of live TUI + DONE.
// URL biased blue so it reads colder than the full ramp
func renderLiveScreenFooter(width int) []string {
	if width < 1 {
		width = tuiDisplayWidth
	}
	right := width - leftPad
	return []string{
		"",
		renderFrostTaglineRight(tuiFooterLine1, right, 0.0, 0.5),
		renderFrostTaglineRight(tuiFooterLine2, right, 0.2, 0.7),
	}
}

// phase renderers. each returns lines, each <= caller width.
// layout: blank, header, blank, 4 stat rows, blank, parsing bar,
// blank, dedup bar, blank, frost tagline footer

func shardWorkerStatus(m *ulpengine.Metrics, r *ulpengine.Resolved) (int64, int64) {
	if r != nil && r.UseFastPath {
		if m.ChunksTotal.Load() == 0 || m.ChunksDone.Load() < m.ChunksTotal.Load() {
			return 1, 1
		}
		return 0, 1
	}
	return int64(m.BusyWorkers.Load()), int64(r.Workers)
}

func shardChunkProgress(m *ulpengine.Metrics, r *ulpengine.Resolved) (float64, int64) {
	total := m.ChunksTotal.Load()
	if total <= 0 {
		return 0, 0
	}
	progress := float64(m.ChunksDone.Load())
	if r != nil && r.TotalInputs > 0 {
		byteProgress := float64(m.BytesRead.Load()) / float64(r.TotalInputs) * float64(total)
		if byteProgress > progress {
			progress = byteProgress
		}
	}
	if progress > float64(total) {
		progress = float64(total)
	}
	return progress, total
}

func renderShardLines(now time.Time, elapsed time.Duration, m *ulpengine.Metrics, r *ulpengine.Resolved, ramMB float64, cpuPct float64, readBPS, shardBPS, regenBPS float64, width int) []string {
	pct := 0.0
	if r.TotalInputs > 0 {
		pct = float64(m.BytesRead.Load()) / float64(r.TotalInputs)
		if pct > 1 {
			pct = 1
		}
	}

	header := renderHeader(spinnerStyle.Render(spinnerFrame(now)), renderStep1PhaseTag(r, m), elapsed, width)

	chunksDigits := numDigits(m.ChunksTotal.Load())
	chunkProgress, chunksTotal := shardChunkProgress(m, r)
	busyWorkers, workerTotal := shardWorkerStatus(m, r)
	workersDigits := numDigits(workerTotal)

	// stat rows w/o per-row indentLine, gradientBox owns framing+indent
	throughput := labelStyle.Render("Throughput") + "   " +
		"read " + byteStyle.Render(padRight(formatRate(readBPS), rateColWidth)) + "    " +
		"shard " + byteStyle.Render(padRight(formatRate(shardBPS), rateColWidth))
	linesInline := labelStyle.Render("Lines") + "        " +
		countStyle.Render(formatCount(m.LinesRead.Load())) + " total " + mutedStyle.Render("·") + " " +
		acceptStyle.Render(formatCount(m.LinesAccepted.Load())) + " accepted " + mutedStyle.Render("·") + " " +
		renderRejected(m.LinesRejected.Load())
	linesStats := []lineStat{
		{"total", formatCount(m.LinesRead.Load()), countStyle},
		{"accepted", formatCount(m.LinesAccepted.Load()), acceptStyle},
		{"rejected", formatCount(m.LinesRejected.Load()), mutedStyle},
	}
	totalBytesStr := humanBytes(r.TotalInputs)
	readBytesStr := padLeft(humanBytes(m.BytesRead.Load()), len(totalBytesStr))
	progressRow := labelStyle.Render("Progress") + "     " +
		byteStyle.Render(readBytesStr) + " / " + byteStyle.Render(totalBytesStr) + "    " +
		"chunks " + countStyle.Render(fmt.Sprintf("%*.1f / %d", chunksDigits+2, chunkProgress, chunksTotal)) + "    " +
		"workers " + countStyle.Render(fmt.Sprintf("%*d / %d busy", workersDigits, busyWorkers, workerTotal))
	systemRow := labelStyle.Render("System") + "       " +
		"RAM " + ramStyle.Render(padRight(humanBytes(int64(ramMB*1024*1024)), bytesColWidth)) + "    " +
		"CPU " + cpuStyle.Render(fmt.Sprintf("%4.0f%%", cpuPct)) + "    " +
		"buckets " + countStyle.Render(fmt.Sprintf("%d", r.BucketCount))

	// gradientBox reserves 2 borders + 4 padding, remaining = inner
	innerW := boxInnerWidth(width)
	innerLines := []string{throughput}
	innerLines = append(innerLines, renderLinesRow(linesInline, linesStats, innerW)...)
	innerLines = append(innerLines, progressRow, systemRow)
	box := gradientBox(innerLines, contentWidth(width), gradStart, gradEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")

	bars := renderMainProgressBars(pct, 0, false, width)
	if r != nil && r.UseFastPath {
		bars = renderFastPathProgressBars(pct, width)
	}

	out := []string{"", header, ""}
	out = append(out, boxLines...)
	out = append(out, "", bars[0], "", bars[1])
	// optional -od phase 0 frame, nil when no dest-dedup work
	if r != nil {
		out = append(out, renderODFrame(r.OdMetrics, regenBPS, width)...)
	}
	out = append(out, renderLiveScreenFooter(width)...)
	return out
}

func renderDedupLines(now time.Time, elapsed time.Duration, m *ulpengine.Metrics, r *ulpengine.Resolved, ramMB float64, cpuPct float64, writeBPS, regenBPS float64, width int) []string {
	bd := m.BucketsDone.Load()
	bt := m.BucketsTotal.Load()
	pct2 := 0.0
	// prefer byte-level progress so bar moves smoothly within a bucket
	// (whole-bucket completions are chunky when N workers start
	// together). falls back to bucket-count ratio when bytes unknown
	if bbT := m.BucketsBytesTotal.Load(); bbT > 0 {
		pct2 = float64(m.BucketsBytesRead.Load()) / float64(bbT)
	} else if bt > 0 {
		pct2 = float64(bd) / float64(bt)
	}
	if pct2 > 1 {
		pct2 = 1
	}

	// inline header: phase tag + optional -od/compress badges, elapsed right
	headerLeft := indentSpace + spinnerStyle.Render(spinnerFrame(now)) + "  " + phaseStyle.Render(renderPhaseTag(r, 2, "DEDUPING"))
	headerLeft += renderDedupHeaderBadges(r)
	headerRight := timeStyle.Render(formatDuration(elapsed))
	headerPad := width - tuiVisibleWidth(headerLeft) - tuiVisibleWidth(headerRight)
	if headerPad < 1 {
		headerPad = 1
	}
	header := headerLeft + strings.Repeat(" ", headerPad) + headerRight

	bucketsDigits := numDigits(bt)
	workersDigits := numDigits(int64(r.DedupWorkers))

	throughput := labelStyle.Render("Throughput") + "   " +
		"write " + byteStyle.Render(padRight(formatRate(writeBPS), rateColWidth))
	linesInline := labelStyle.Render("Lines") + "        " +
		uniqueStyle.Render(formatCount(m.LinesUnique.Load())) + " unique so far " + mutedStyle.Render("·") + " " +
		renderRejected(m.LinesRejected.Load()) + " " + mutedStyle.Render("(final)")
	linesStats := []lineStat{
		{"unique so far", formatCount(m.LinesUnique.Load()), uniqueStyle},
		{"rejected (final)", formatCount(m.LinesRejected.Load()), mutedStyle},
	}
	progressRow := labelStyle.Render("Progress") + "     " +
		"buckets " + countStyle.Render(fmt.Sprintf("%*d / %d", bucketsDigits, bd, bt)) + "    " +
		"workers " + countStyle.Render(fmt.Sprintf("%*d / %d busy", workersDigits, m.BusyWorkers.Load(), r.DedupWorkers))
	systemRow := labelStyle.Render("System") + "       " +
		"RAM " + ramStyle.Render(padRight(humanBytes(int64(ramMB*1024*1024)), bytesColWidth)) + "    " +
		"CPU " + cpuStyle.Render(fmt.Sprintf("%4.0f%%", cpuPct))

	innerW := boxInnerWidth(width)
	innerLines := []string{throughput}
	innerLines = append(innerLines, renderLinesRow(linesInline, linesStats, innerW)...)
	innerLines = append(innerLines, progressRow, systemRow)
	// -od: live library index scan while each bucket's dest set is loaded
	if r != nil && r.Cfg.DestDedup && r.OdMetrics != nil {
		if total := r.OdMetrics.KeysTotalEstimate.Load(); total > 0 {
			done := r.OdMetrics.KeysLoaded.Load()
			if done > total {
				done = total
			}
			innerLines = append(innerLines, renderLibraryMatchingRows(done, total, innerW)...)
		}
	}
	box := gradientBox(innerLines, contentWidth(width), gradStart, gradEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")

	bars := renderMainProgressBars(1.0, pct2, true, width)

	out := []string{"", header, ""}
	out = append(out, boxLines...)
	out = append(out, "", bars[0], "", bars[1])
	if r != nil {
		out = append(out, renderODFrame(r.OdMetrics, regenBPS, width)...)
	}
	out = append(out, renderLiveScreenFooter(width)...)
	return out
}

func outputPathsForUI(r *ulpengine.Resolved) []string {
	if r == nil {
		return nil
	}
	if len(r.OutputPaths) > 0 {
		return r.OutputPaths
	}
	if strings.TrimSpace(r.Cfg.Output) != "" {
		return []string{r.Cfg.Output}
	}
	return nil
}

// final summary bordered block. box sized to (width-leftPad-2) so right
// edge stays inside the grid after 4-col indent.
// for full post-success stdout use renderFinalStdoutSummary
func renderDoneLines(elapsed time.Duration, m *ulpengine.Metrics, r *ulpengine.Resolved, width int) []string {
	uniq := m.LinesUnique.Load()
	rej := m.LinesRejected.Load()
	skippedByDest := m.LinesSkippedByDest.Load()
	// genuine within-run dups = parsed cleanly - unique - library hits.
	// w/o dest subtraction a -od run double-counts library hits
	dup := m.LinesAccepted.Load() - uniq - skippedByDest
	if dup < 0 {
		dup = 0
	}

	header := renderHeader(okStyle.Render("✓"), "COMPLETE", elapsed, width)

	// "Output" reports on-disk size. -zst appends "(N.NNx compressed)"
	// against the byte counter (uncompressed input to encoder). stat
	// failure falls back to counter
	outBytes := m.BytesWritten.Load()
	outDisplay := humanBytes(outBytes)
	var ratioNote string
	if r.Cfg.Compress {
		paths := outputPathsForUI(r)
		if len(paths) > 0 {
			var diskSize int64
			for _, p := range paths {
				if fi, err := os.Stat(p); err == nil {
					diskSize += fi.Size()
				}
			}
			if diskSize > 0 {
				outDisplay = humanBytes(diskSize)
				if diskSize > 0 && outBytes > 0 {
					ratio := float64(outBytes) / float64(diskSize)
					ratioNote = fmt.Sprintf("  (%.2fx compressed)", ratio)
				}
			}
		}
	}

	// path rendered below the frame via renderDoneOutputFooter so long
	// absolute paths stay copy-paste friendly

	// "Removed" surfaces 3 categories: rejects, in-run dups, -od
	// library hits. each bullet only renders when non-zero.
	// multi-bullet single line can exceed inner width on 8-9 digit
	// counts, fall back to one bullet/row
	var removedBullets []string
	if rej > 0 {
		removedBullets = append(removedBullets,
			warnStyle.Render(formatCount(rej))+" "+mutedStyle.Render("rejected"))
	}
	if dup > 0 {
		removedBullets = append(removedBullets,
			countStyle.Render(formatCount(dup))+" "+mutedStyle.Render("duplicates"))
	}
	if skippedByDest > 0 {
		removedBullets = append(removedBullets,
			countStyle.Render(formatCount(skippedByDest))+" "+mutedStyle.Render("already in library"))
	}
	removedRows := renderRemovedRows(removedBullets, boxInnerWidth(width))

	innerLines := []string{
		labelStyle.Render("Input    ") + byteStyle.Render(humanBytes(r.TotalInputs)) +
			"  " + mutedStyle.Render("across") + "  " +
			countStyle.Render(fmt.Sprintf("%d", r.InputFileCount)) + " " + mutedStyle.Render("files"),
		labelStyle.Render("Lines    ") + countStyle.Render(formatCount(m.LinesRead.Load())) +
			" " + mutedStyle.Render("read"),
		labelStyle.Render("Output   ") + byteStyle.Render(outDisplay) + mutedStyle.Render(ratioNote),
		labelStyle.Render("Unique   ") + uniqueStyle.Render(formatCount(uniq)) + " " + mutedStyle.Render("entries"),
	}
	// rejected uses warnStyle on final recap (vs muted in live) b/c
	// number IS the actionable outcome here. label stays muted
	innerLines = append(innerLines, removedRows...)

	box := gradientBox(innerLines, contentWidth(width), doneStart, doneEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")

	out := []string{"", header, ""}
	out = append(out, boxLines...)
	return out
}

// everything printed post-success on main screen: COMPLETE frame, optional
// -od library recap, output path row, frost tagline footer
func renderFinalStdoutSummary(elapsed time.Duration, m *ulpengine.Metrics, r *ulpengine.Resolved, width int, notice *selfupdate.Notice) []string {
	var out []string
	// -del paths before DONE summary so long delete lists dont
	// push stats off-screen
	out = append(out, renderDoneDeletedFooter(r)...)
	out = append(out, renderDoneLines(elapsed, m, r, width)...)
	if odLines := renderODSummary(r, m, width); len(odLines) > 0 {
		out = append(out, odLines...)
	}
	out = append(out, renderDoneOutputFooter(r)...)
	out = append(out, renderSummaryFooter(width, notice)...)
	if skipLines := renderODSkippedPaths(r, width); len(skipLines) > 0 {
		out = append(out, skipLines...)
	}
	return out
}

// "1 archive" or "N archives", reads as English not "1 archives"
func pluralizeArchives(n int) string {
	if n == 1 {
		return "1 archive"
	}
	return fmt.Sprintf("%d archives", n)
}

// libraryLineCountTotal is the indexed line count across the whole library
// after this run: prior archives (phase 0) plus unique lines just written.
func libraryLineCountTotal(res *ulpengine.ODResult, m *ulpengine.Metrics) int64 {
	total := int64(res.TotalKeysLoaded)
	if m != nil {
		total += m.LinesUnique.Load()
	}
	return total
}

// post-run library recap shown below COMPLETE frame when -od used.
// nil when no odResult or empty library (first -od run).
// intentionally minimal so multi-billion entry libraries never ellipsise
func renderODSummary(r *ulpengine.Resolved, m *ulpengine.Metrics, width int) []string {
	if r == nil || r.OdResult == nil {
		return nil
	}
	res := r.OdResult
	if res.ArchivesTotal == 0 {
		return nil
	}

	innerLines := []string{
		uniqueStyle.Render(formatCount(libraryLineCountTotal(res, m))),
		mutedStyle.Render("lines in library"),
	}
	if res.ArchivesUpgraded > 0 {
		innerLines = append(innerLines,
			warnStyle.Render(fmt.Sprintf("Index format upgraded (one-time, %d parts)", res.ArchivesUpgraded)),
		)
	}

	box := gradientBox(innerLines, contentWidth(width), gradStart, gradEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")

	out := []string{"", ""}
	out = append(out, boxLines...)
	return out
}

// per-archive skipped paths AFTER alt-screen teardown. stderr writes
// during phase 0 get wiped, so user has no way to find skipped files
// w/o -debug. capped at 5, "and N more" trailer
func renderODSkippedPaths(r *ulpengine.Resolved, width int) []string {
	if r == nil || r.OdResult == nil {
		return nil
	}
	paths := r.OdResult.SkippedArchivePaths
	if len(paths) == 0 {
		return nil
	}
	const limit = 5
	out := []string{
		"",
		indentSpace + warnStyle.Render(fmt.Sprintf("Skipped archives during indexing (%d):", len(paths))),
	}
	shown := paths
	if len(shown) > limit {
		shown = shown[:limit]
	}
	for _, p := range shown {
		out = append(out, indentSpace+"  "+mutedStyle.Render("·")+"  "+p)
	}
	if extra := len(paths) - len(shown); extra > 0 {
		out = append(out, indentSpace+"  "+mutedStyle.Render(fmt.Sprintf("· and %d more", extra)))
	}
	return out
}

// one row per output file below DONE frame. same label width/colors as
// inside the box. additional archives align under the first path.
// paths NOT padOrTrim'd, can span full terminal width
func renderDoneOutputFooter(r *ulpengine.Resolved) []string {
	if r == nil {
		return nil
	}
	const doneOutputFooterLabel = "Output   "
	mid := doneStart.BlendLuv(doneEnd, 0.5)
	border := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	labelCell := labelStyle.Render(doneOutputFooterLabel)
	labelW := lipgloss.Width(labelCell)
	prefix := strings.Repeat(" ", leftPad) + border.Render("┃") + "  "
	blankLabel := strings.Repeat(" ", labelW)

	// Post-run r.OutputPaths is authoritative (this footer only renders on
	// success): empty means every line was rejected or already in the library
	// and the engine discarded the generated shard. Show an explicit note rather
	// than a path to a removed file -- and don't use outputPathsForUI here, whose
	// live fallback to Cfg.Output would resurrect that path.
	if len(r.OutputPaths) == 0 {
		return []string{"", prefix + labelCell + mutedStyle.Render("(nothing new)")}
	}
	paths := r.OutputPaths

	out := []string{""}
	for i, p := range paths {
		pathCell := phaseStyle.Render(p)
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

// one row per -del'd input. same gutter/label column as output footer
func renderDoneDeletedFooter(r *ulpengine.Resolved) []string {
	if r == nil || len(r.DeletedInputPaths) == 0 {
		return nil
	}
	const doneDeletedFooterLabel = "Deleted  "
	mid := doneStart.BlendLuv(doneEnd, 0.5)
	border := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	labelCell := labelStyle.Render(doneDeletedFooterLabel)
	labelW := lipgloss.Width(labelCell)
	prefix := strings.Repeat(" ", leftPad) + border.Render("┃") + "  "
	blankLabel := strings.Repeat(" ", labelW)

	out := []string{""}
	for i, p := range r.DeletedInputPaths {
		pathCell := warnStyle.Render(p)
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

// "[!]" badge on interrupt frame header
var interruptWarnStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})

// "Destination dedup" frame stacked under main frame during -od phase 0.
// nil when m is nil or phase idle/done.
// layout: gradient-bordered box w/ 2-3 stat lines + progress bar.
// frost-blue → icy-white border reads as "background work" vs main's
// purple-pink "active phase"
//
// renderPhase0Lines is the primary panel when phase 0 is in flight.
// shown after parsing finishes while library sidecar work continues.
func renderPhase0Lines(elapsed time.Duration, m *ulpengine.Metrics, r *ulpengine.Resolved, ramMB float64, cpuPct float64, regenBPS float64, width int) []string {
	now := time.Now()
	header := renderHeader(spinnerStyle.Render(spinnerFrame(now)), renderPhaseTag(r, 1, "LIBRARY PREP"), elapsed, width)

	odLines := renderODFrame(r.OdMetrics, regenBPS, width)
	// renderODFrame leads w/ blank for spacing under main frame.
	// in phase-0-primary mode the header above already provides it
	if len(odLines) > 0 && odLines[0] == "" {
		odLines = odLines[1:]
	}

	out := []string{"", header, ""}
	out = append(out, odLines...)

	// system row matches other phase frames so RAM/CPU stay visible
	systemRow := labelStyle.Render("System") + "       " +
		"RAM " + ramStyle.Render(padRight(humanBytes(int64(ramMB*1024*1024)), bytesColWidth)) + "    " +
		"CPU " + cpuStyle.Render(fmt.Sprintf("%4.0f%%", cpuPct))
	out = append(out, "", indentSpace+systemRow)
	out = append(out, renderLiveScreenFooter(width)...)
	return out
}

// primary panel during phaseIndex (post-dedup own-output sidecar pass).
// mirrors renderPhase0Lines so user sees a coherent regen frame across phases
func renderIndexLines(elapsed time.Duration, m *ulpengine.Metrics, r *ulpengine.Resolved, ramMB float64, cpuPct float64, regenBPS float64, width int) []string {
	now := time.Now()
	header := renderHeader(spinnerStyle.Render(spinnerFrame(now)), renderPhaseTagWithTotal(3, 3, "INDEXING OUTPUT"), elapsed, width)

	var odMetricsForFrame *ulpengine.ODMetrics
	if r != nil {
		odMetricsForFrame = r.OutputIdxMetrics
	}
	odLines := renderODFrame(odMetricsForFrame, regenBPS, width)
	if len(odLines) > 0 && odLines[0] == "" {
		odLines = odLines[1:]
	}

	out := []string{"", header, ""}
	out = append(out, odLines...)

	systemRow := labelStyle.Render("System") + "       " +
		"RAM " + ramStyle.Render(padRight(humanBytes(int64(ramMB*1024*1024)), bytesColWidth)) + "    " +
		"CPU " + cpuStyle.Render(fmt.Sprintf("%4.0f%%", cpuPct))
	out = append(out, "", indentSpace+systemRow)
	out = append(out, renderLiveScreenFooter(width)...)
	return out
}

// floor for per-worker rows in OD frame. dropping below 8 erodes the
// "live activity" signal. ceiling computed adaptively via workerRowCap
const maxWorkerRowsRendered = 8

// row budget OD frame must leave for non-worker content in phase-0
// primary layout: blank+header+blank, frame box, main bar, System row,
// footer. 18 covers typical phase 0 w/ safety margin for SIGWINCH shrink
const phase0ReservedRows = 18

// per-worker row count given terminal height and total worker slots.
// pure fn so tests can pass arbitrary heights.
// never more than totalWorkers, never fewer than maxWorkerRowsRendered,
// expands toward totalWorkers when terminal is tall enough
func workerRowCap(termHeight, totalWorkers int) int {
	if totalWorkers <= 0 {
		return 0
	}
	available := termHeight - phase0ReservedRows
	if available < maxWorkerRowsRendered {
		available = maxWorkerRowsRendered
	}
	if available > totalWorkers {
		available = totalWorkers
	}
	return available
}

func renderODFrame(m *ulpengine.ODMetrics, regenBPS float64, width int) []string {
	if m == nil {
		return nil
	}
	phase := ulpengine.ODPhase(m.Phase.Load())
	if phase == ulpengine.ODPhaseIdle || phase == ulpengine.ODPhaseDone {
		return nil
	}

	archivesTotal := m.ArchivesTotal.Load()
	filesTotal := m.FilesTotal.Load()
	archivesNeedRegen := m.ArchivesNeedRegen.Load()
	archivesRegenedDone := m.ArchivesRegenedDone.Load()
	archivesSkipped := m.ArchivesSkipped.Load()
	partsRegenTotal := m.PartsRegenTotal.Load()
	partsRegenDone := m.PartsRegenDone.Load()
	regenBytesTotal := m.RegenBytesTotal.Load()
	regenBytesRead := m.RegenBytesRead.Load()
	// per-part sidecars finalize inline at end of each task, so no
	// "streaming done, finalizing sidecars" sub-phase. 100% bytes = done

	var phaseDesc string
	switch phase {
	case ulpengine.ODPhaseDiscover:
		phaseDesc = "scanning library"
		if m.PartsUpgradeTotal.Load() > 0 {
			phaseDesc = "scanning library · legacy index detected"
		}
	case ulpengine.ODPhaseRegen:
		phaseDesc = "indexing archives + writing .idx"
	case ulpengine.ODPhaseUpgrade:
		phaseDesc = "upgrading index format (v2→v3)"
	case ulpengine.ODPhaseIndexOwn:
		phaseDesc = "indexing this run's output"
	}

	// header label flips for post-dedup output-index pass so frame
	// doesnt claim to be doing dest-dedup work (library long closed)
	frameTitle := "Destination dedup"
	if phase == ulpengine.ODPhaseIndexOwn {
		frameTitle = "Output index"
	}
	headerLine := labelStyle.Render(frameTitle)
	if phaseDesc != "" {
		headerLine += " " + mutedStyle.Render("· "+phaseDesc)
	}

	// library row: archive count + on-disk file count when they differ
	// + indexing progress. "1 archive across 16 files" disarms the
	// mismatch users get from `ls` showing many .zst files.
	//
	// during regen prefer parts denominator over archives (parallel
	// per-part workers, archive counter only flips at end of run, so
	// 1-run x 16-parts sits at "0/1" for ~99% of phase 0)
	libRow := labelStyle.Render("Library     ") + countStyle.Render(pluralizeArchives(int(archivesTotal)))
	if filesTotal > 0 && filesTotal > archivesTotal {
		libRow += " " + mutedStyle.Render(fmt.Sprintf("across %d files", filesTotal))
	}
	if partsRegenTotal > 0 {
		libRow += " " + mutedStyle.Render("·") + " " +
			countStyle.Render(fmt.Sprintf("%d / %d parts indexed", partsRegenDone, partsRegenTotal))
	} else if archivesNeedRegen > 0 {
		// legacy paths w/o parts counter, archive-grained label
		libRow += " " + mutedStyle.Render("·") + " " +
			countStyle.Render(fmt.Sprintf("%d / %d indexing", archivesRegenedDone, archivesNeedRegen))
	}
	if archivesSkipped > 0 {
		libRow += " " + mutedStyle.Render("·") + " " +
			warnStyle.Render(fmt.Sprintf("%d skipped", archivesSkipped))
	}
	if phase == ulpengine.ODPhaseDiscover && m.PartsUpgradeTotal.Load() > 0 {
		libRow += " " + mutedStyle.Render("·") + " " +
			warnStyle.Render("one-time upgrade next")
	}

	innerLines := []string{headerLine, libRow}

	// phase-specific second row
	switch phase {
	case ulpengine.ODPhaseDiscover:
		if m.PartsUpgradeTotal.Load() > 0 {
			innerLines = append(innerLines, labelStyle.Render("Note        ")+
				warnStyle.Render("Legacy index format · in-place upgrade runs once, then skipped"))
		}
	case ulpengine.ODPhaseRegen, ulpengine.ODPhaseIndexOwn:
		if regenBytesTotal > 0 {
			innerLines = append(innerLines, labelStyle.Render("Bytes       ")+
				byteStyle.Render(humanBytes(regenBytesRead))+
				" / "+byteStyle.Render(humanBytes(regenBytesTotal)))
		}
		// throughput row stays present for whole byte-indexing phase
		// once denominator is known. byte deltas can sample 0 between
		// decoder reads, hiding the row would change frame height
		if regenBytesTotal > 0 {
			remaining := regenBytesTotal - regenBytesRead
			eta := ""
			if remaining > 0 && regenBPS > 1 {
				secs := float64(remaining) / regenBPS
				eta = " " + mutedStyle.Render("· ETA "+formatDuration(time.Duration(secs)*time.Second))
			}
			innerLines = append(innerLines, labelStyle.Render("Throughput  ")+
				byteStyle.Render(formatRate(regenBPS))+eta)
		}
	case ulpengine.ODPhaseUpgrade:
		innerLines = append(innerLines,
			labelStyle.Render("Important   ")+
				warnStyle.Render("One-time library upgrade — please wait, do not interrupt (Ctrl+C)"),
			labelStyle.Render("Mode        ")+
				mutedStyle.Render("in-place re-sort · archives not read"),
			labelStyle.Render("Safety      ")+
				mutedStyle.Render("your .zst archives are safe · only index files are updated"),
		)
	}

	box := gradientBox(innerLines, contentWidth(width), frostGradA, frostGradB)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")

	// per-worker rows OUTSIDE the frame, between box and main bar so
	// phase 0 mirrors phase 1/2 layout (info box on top, stack below).
	// inside-the-frame mini bars made the box feel cluttered.
	//
	// only shown when there's per-worker BYTE progress (archive decompression).
	// the in-place v2->v3 upgrade (migration) has no byte denominator, so its
	// rows would sit frozen — there we fall through to a single aggregate bar
	// below (parts-indexed), like the old routing bar.
	var workerBars []string
	if (phase == ulpengine.ODPhaseRegen || phase == ulpengine.ODPhaseIndexOwn) && regenBytesTotal > 0 {
		rowWidth := contentWidth(width)
		cap := workerRowCap(termHeight(), m.WorkerCount())
		active := m.ActiveWorkers(cap)
		// idx marker width must fit the WIDEST displayed index ("[16]"
		// is 5, "[8]" is 4). otherwise rows 10+ shift right by 1
		idxW := workerIdxMarkerWidth(len(active))
		for i, ws := range active {
			workerBars = append(workerBars, indentSpace+renderWorkerRow(i, ws, rowWidth, idxW))
		}
	}

	// single progress bar below the frame. byte-based during archive regen;
	// falls back to parts-indexed for the upgrade/migration pass (no bytes).
	var pct float64
	switch phase {
	case ulpengine.ODPhaseRegen, ulpengine.ODPhaseIndexOwn:
		if regenBytesTotal > 0 {
			pct = float64(regenBytesRead) / float64(regenBytesTotal)
		} else if partsRegenTotal > 0 {
			pct = float64(partsRegenDone) / float64(partsRegenTotal)
		}
	case ulpengine.ODPhaseUpgrade:
		if partsRegenTotal > 0 {
			pct = float64(partsRegenDone) / float64(partsRegenTotal)
		}
	}
	if pct > 1 {
		pct = 1
	}
	bar := indentSpace + gradientBar(pct, contentWidth(width))

	// blank gap above OD frame separates it from main frame's bottom bar
	out := []string{""}
	out = append(out, boxLines...)
	if len(workerBars) > 0 {
		out = append(out, "")
		out = append(out, workerBars...)
	}
	out = append(out, "", bar)
	return out
}

// per-worker row layout. bars aligned to a single column so the eye
// can scan progress vertically. 22-cell bar reads as substantial
// w/o pushing bytes column off-screen on 100-col terminals
const (
	workerBarBodyW = 22
	workerPctW     = 4 // " XX%" or " ?? %"
	// each humanBytes gets a fixed slot, "999.9 GB" is widest reading
	workerByteW = 8
)

// column width for "[N] " idx marker so it stays constant across rows.
// count = number of rows being rendered. floor of 4 keeps single-digit
// layouts visually identical to pre-adaptive era
func workerIdxMarkerWidth(count int) int {
	if count < 1 {
		return 4
	}
	// "[" + digits(count) + "] "
	return 1 + numDigits(int64(count)) + 2
}

// one per-worker line for OD frame. column-aligned across rows:
//
//	"[1] xyz_part04   (4/16)  ████████░░░░░░░░ 36%   1.0 GB / 2.1 GB"
//
// name + part padded to fixed left-width so bars start at same column.
// atomic loads taken once at entry, worst case is stale-by-one-tick "97%"
func renderWorkerRow(idx int, ws *ulpengine.WorkerStatus, innerWidth, idxMarkerW int) string {
	namePtr := ws.ArchivePath.Load()
	if namePtr == nil {
		return ""
	}
	name := *namePtr
	partIdx := ws.PartIdx.Load()
	partsTotal := ws.PartsTotal.Load()
	bytesDone := ws.BytesDone.Load()
	bytesTotal := ws.BytesTotal.Load()

	// trim sfu prefix + .txt.zst suffix so part id reads tightly.
	// falls back to raw name on non-matching convention
	displayName := compactArchiveName(name)

	var pct float64
	if bytesTotal > 0 {
		pct = float64(bytesDone) / float64(bytesTotal)
		if pct > 1 {
			pct = 1
		}
	}
	bar := miniGradientBar(pct, workerBarBodyW, frostGradA, frostGradB)
	var pctText string
	if bytesTotal > 0 {
		pctText = fmt.Sprintf("%3d%%", int(pct*100))
	} else {
		pctText = "  ?%"
	}

	partAnnot := ""
	if partsTotal > 1 {
		partAnnot = fmt.Sprintf("(%d/%d)", partIdx, partsTotal)
	}

	// bytes column: each value padded to workerByteW so "/" and
	// totals stack vertically. skipped when size unknown (first tick)
	var bytesText string
	if bytesTotal > 0 {
		bytesText = padLeft(humanBytes(bytesDone), workerByteW) + " / " + padLeft(humanBytes(bytesTotal), workerByteW)
	}

	// reserve right-side cols, rest = left-section width.
	// layout: "[N] LEFT  BAR pct  BYTES"
	rightW := 2 /*gutter*/ + workerBarBodyW + 1 /*space*/ + workerPctW + 2 /*gutter*/ + tuiVisibleWidth(bytesText)
	leftW := innerWidth - idxMarkerW - rightW
	if leftW < 12 {
		leftW = 12 // floor, name must stay readable
	}

	// compose left: name + space + partAnnot, padded to leftW
	leftPlain := displayName
	if partAnnot != "" {
		leftPlain += " " + partAnnot
	}
	if tuiVisibleWidth(leftPlain) > leftW {
		// truncate name, keep partAnnot so user always sees which
		// part of the run is in flight
		suffix := ""
		if partAnnot != "" {
			suffix = " " + partAnnot
		}
		nameMax := leftW - tuiVisibleWidth(suffix)
		if nameMax < 4 {
			nameMax = 4
		}
		if len(displayName) > nameMax {
			displayName = "..." + displayName[len(displayName)-nameMax+3:]
		}
		leftPlain = displayName + suffix
	}

	var styledLeft strings.Builder
	styledLeft.WriteString(countStyle.Render(displayName))
	if partAnnot != "" {
		styledLeft.WriteString(" ")
		styledLeft.WriteString(mutedStyle.Render(partAnnot))
	}
	// padding unstyled, mutedStyle would tint BG on some terminals
	if pad := leftW - tuiVisibleWidth(leftPlain); pad > 0 {
		styledLeft.WriteString(strings.Repeat(" ", pad))
	}

	// pad idx marker to idxMarkerW so single/double digits land same col
	marker := fmt.Sprintf("[%d]", idx+1)
	if pad := idxMarkerW - tuiVisibleWidth(marker); pad > 0 {
		marker += strings.Repeat(" ", pad)
	}
	return fmt.Sprintf("%s%s  %s %s  %s",
		marker,
		styledLeft.String(),
		bar,
		mutedStyle.Render(pctText),
		mutedStyle.Render(bytesText))
}

// small block-character progress bar w/ per-cell gradient. distinct
// from main gradientBar: glyph ▆ (not █) so worker bars dont compete
// w/ main bar below them, no embedded percent (caller aligns it)
func miniGradientBar(percent float64, width int, start, end colorful.Color) string {
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
		// stretch across full bar (not just filled), matches gradientBar
		t := 0.0
		if width > 1 {
			t = float64(i) / float64(width-1)
		}
		c := start.BlendLuv(end, t)
		b.WriteString(lipgloss.NewStyle().
			Foreground(lipgloss.Color(c.Hex())).
			Render("▆"))
	}
	if rem := width - fill; rem > 0 {
		b.WriteString(emptyStyle.Render(strings.Repeat("░", rem)))
	}
	return b.String()
}

// trims sfu_ prefix and .txt.zst suffix from stamp-named archives so
// per-worker rows stay readable. returns input unchanged on non-match
func compactArchiveName(name string) string {
	out := name
	if strings.HasSuffix(out, ".txt.zst") {
		out = out[:len(out)-len(".txt.zst")]
	}
	if strings.HasPrefix(out, "sfu_") {
		out = out[len("sfu_"):]
	}
	return out
}

// "cleaning up after Ctrl-C" notice in place of active phase frame.
// cleanupLog lines render full-width above the box (muted grey), matching sfl's
// issue block above the summary frame.
func renderInterruptLines(elapsed time.Duration, width int, cleanupLog []string) []string {
	header := renderHeader(interruptWarnStyle.Render("[!]"), "INTERRUPTED — cleaning up", elapsed, width)

	out := []string{"", header}
	if block := renderCleanupLogAbove(cleanupLog, termWidthFull()); len(block) > 0 {
		out = append(out, "")
		out = append(out, block...)
	}

	innerLines := []string{
		"Flushing output and removing temp shards.",
		"This usually takes a few seconds; please wait.",
		"",
		mutedStyle.Render("A second Ctrl+C will force-exit without cleanup."),
	}

	box := gradientBox(innerLines, contentWidth(width), interruptStart, interruptEnd)
	boxLines := strings.Split(indentBlock(box, leftPad), "\n")

	out = append(out, "")
	out = append(out, boxLines...)
	out = append(out, renderLiveScreenFooter(width)...)
	return out
}

// renderCleanupLogAbove is grey, full-terminal-width cleanup narration printed
// above the interrupt box. Uses leftPad so the block aligns with the header.
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
