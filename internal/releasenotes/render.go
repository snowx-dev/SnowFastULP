package releasenotes

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"

	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
)

// Palette copied from cmd/sfu/tui.go:59-113 — that file is the source of
// truth for the purple gradient pair (gradStart/gradEnd at tui.go:111-113)
// and the shared adaptive styles (mutedStyle, byteStyle, phaseStyle). The
// hexes are already duplicated per cmd; this copy documents the same
// provenance rather than introducing a new color. The ramp colors stay a
// Render parameter so a future sfl-specific tint (#9E3A6E→#DD2E44,
// cmd/sfl/tui.go:68-69) is a one-line change; v0.3 ships sfu purple for all
// three tools.
var (
	notesGradStart, _ = colorful.Hex("#5A56E0") // purple
	notesGradEnd, _   = colorful.Hex("#EE6FF8") // pink

	// headings + box title: bold, flat ramp-start purple
	headingStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5A56E0"))

	// bullet markers, soft-failure line: the TUI's muted gray
	unavailableStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
	bulletMarkStyle  = unavailableStyle

	// inline `code` spans: amber
	byteStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "178", Dark: "222"})

	// bold spans and the footer URL (the TUI's phase-tag color)
	boldStyle  = lipgloss.NewStyle().Bold(true)
	phaseStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "33", Dark: "51"})
)

// Render fetch-free rendering entry point: parses the (already sanitized)
// markdown-lite notes and writes the decorated box to out, wrapped to
// width terminal cells. Ramp colors are parameters; ShowNotes passes the
// sfu purple pair.
//
// Render relies on lipgloss's default renderer, which self-detects its
// color profile from os.Stdout on first use — the update dispatch runs
// before any applyStderrColorProfile call, so piped stdout degrades to
// Ascii (geometry intact) and TTY stdout gets true color. If a future
// change ever routes update output after applyStderrColorProfile, colors
// would follow stderr instead and this assumption breaks.
func Render(out io.Writer, notes, version string, termWidth int, start, end colorful.Color) error {
	outer := termWidth - 2*leftPad
	var text string
	if outer < narrowThreshold {
		text = plainNotes(entryRows(parseBlocks(notes), innerWidthFor(outer)), version, outer)
	} else {
		text = notesBox(entryRows(parseBlocks(notes), outer-6), version, outer, start, end)
	}
	_, err := io.WriteString(out, text+"\n")
	return err
}

// innerWidthFor maps a plain-fallback outer width to the wrap width (the
// same outer-6 budget the box uses, so both paths agree).
func innerWidthFor(outer int) int {
	if w := outer - 6; w > 0 {
		return w
	}
	return 1
}

// --- markdown-lite parser (design §D2) ---

type segStyle uint8

const (
	segPlain segStyle = iota
	segBold
	segCode
)

type segment struct {
	text  string
	style segStyle
}

type blockKind uint8

const (
	blockBlank        blockKind = iota // paragraph break
	blockHeading                       // "### Heading"
	blockBullet                        // "- item" → "· item"
	blockContinuation                  // 2-space-indented bullet continuation
	blockPlain                         // everything else
)

type block struct {
	kind     blockKind
	segments []segment
}

var (
	headingRe  = regexp.MustCompile(`^(#{1,6}) `)
	linkRe     = regexp.MustCompile(`\[[^\[\]]*\]\([^()]*\)`)
	numberedRe = regexp.MustCompile(`^[0-9]+\. `)
	// htmlTagRe strips real HTML tags only. Notes files legitimately use
	// `<version>`-style angle-bracket placeholders, which must survive, so
	// the pattern matches known tag names rather than any <word>.
	htmlTagRe = regexp.MustCompile(`(?i)</?(?:p|br|b|i|em|strong|code|pre|ul|ol|li|h[1-6]|blockquote|details|summary|sub|sup|hr)(?:\s[^>]*)?/?>`)
)

func stripHTML(s string) string { return htmlTagRe.ReplaceAllString(s, "") }

// stripLinks reduces "[text](url)" to "text".
func stripLinks(s string) string {
	return linkRe.ReplaceAllStringFunc(s, func(m string) string {
		return m[1:strings.Index(m, "]")]
	})
}

// parseInline splits text into bold/code/plain segments. Any unbalanced `**`
// or backtick demotes the whole text to one plain segment with the literal
// markers kept — the §D2 degrade rule: unbalanced markers never silently
// disappear.
func parseInline(text string) []segment {
	if strings.Count(text, "`")%2 != 0 || strings.Count(text, "**")%2 != 0 {
		return []segment{{text: text}}
	}
	var segs []segment
	for i, part := range strings.Split(text, "`") {
		if i%2 == 1 { // inside a code span: literal, not re-parsed
			if part != "" {
				segs = append(segs, segment{text: part, style: segCode})
			}
			continue
		}
		for j, b := range strings.Split(part, "**") {
			if b == "" {
				continue
			}
			style := segPlain
			if j%2 == 1 {
				style = segBold
			}
			segs = append(segs, segment{text: b, style: style})
		}
	}
	if len(segs) == 0 {
		segs = []segment{{text: ""}}
	}
	return segs
}

// parseBlocks turns sanitized markdown-lite notes into a flat block list,
// supporting exactly the constructs notes files use: ### headings, -
// bullets with 2-space continuations, **bold**, `code`, numbered items,
// blank-line breaks, and nothing else deliberately. `##`/`#` title lines
// are skipped (the extraction already drops the section's own heading).
//
// A bullet's 2-space-indented continuation lines are soft wraps of one
// paragraph: they merge into the bullet's text flow (joined with single
// spaces) before inline parsing, so the whole bullet greedily wraps at the
// inner width instead of rendering the source's own ~90-column hard wraps
// as ragged orphan rows. Blank lines, headings, new bullets, prose, and EOF
// close the open bullet. Fenced blocks keep their degrade rules unchanged:
// column-0 fences toggle as before, and fence content inside an open bullet
// still demotes to plain indented continuation lines, never dropped.
func parseBlocks(notes string) []block {
	var blocks []block
	inFence, inBullet := false, false
	var bulletParts []string
	add := func(kind blockKind, text string) {
		blocks = append(blocks, block{kind: kind, segments: parseInline(stripLinks(stripHTML(text)))})
	}
	// emit the pending merged bullet text as a bullet block; the bullet
	// stays logically open unless closeBullet runs (fences interleave).
	emitBullet := func() {
		if len(bulletParts) > 0 {
			add(blockBullet, strings.Join(bulletParts, " "))
			bulletParts = nil
		}
	}
	closeBullet := func() {
		emitBullet()
		inBullet = false
	}
	for _, line := range strings.Split(notes, "\n") {
		switch {
		case strings.HasPrefix(line, "```"):
			emitBullet() // order: the bullet block precedes the fence content
			inFence = !inFence
		case inFence:
			// fenced content demotes to plain (indented under the open bullet)
			if line == "" {
				blocks = append(blocks, block{kind: blockBlank})
			} else {
				kind := blockPlain
				if inBullet {
					kind = blockContinuation
				}
				blocks = append(blocks, block{kind: kind, segments: []segment{{text: line}}})
			}
		case line == "":
			closeBullet()
			blocks = append(blocks, block{kind: blockBlank})
		default:
			if m := headingRe.FindStringSubmatch(line); m != nil {
				closeBullet()
				if len(m[1]) == 3 {
					add(blockHeading, line[4:])
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "- "):
				closeBullet()
				bulletParts, inBullet = []string{line[2:]}, true
			case inBullet && strings.HasPrefix(line, "  "):
				if len(bulletParts) == 0 {
					// residue after a mid-bullet fence keeps the old
					// per-line continuation shape
					add(blockContinuation, line[2:])
					continue
				}
				bulletParts = append(bulletParts, line[2:])
			default:
				closeBullet()
				// numbered lists ("1. item") and prose pass through plain,
				// markers kept
				add(blockPlain, line)
			}
		}
	}
	closeBullet()
	// collapse blank runs, trim leading/trailing blanks
	var out []block
	for _, b := range blocks {
		if b.kind == blockBlank && (len(out) == 0 || out[len(out)-1].kind == blockBlank) {
			continue
		}
		out = append(out, b)
	}
	if n := len(out); n > 0 && out[n-1].kind == blockBlank {
		out = out[:n-1]
	}
	return out
}

// --- wrapping ---

// wrapSegments word-wraps the concatenation of the segment texts at limit
// cells with ansi.WordwrapWc (wc = wide-rune aware, "/" added as a
// breakpoint so URLs break at slashes; "-" is always a breakpoint), then
// re-applies the segment styles to each wrapped row. A segment split at the
// wrap column re-opens its SGR in the continuation piece, because each row
// is styled from its own segments. Rows may exceed limit by one breakpoint
// cell (the wrapper emits the breakpoint before breaking); the box's
// PadOrTrim row fit absorbs that.
func wrapSegments(segs []segment, limit int) [][]segment {
	if limit < 1 {
		limit = 1
	}
	var plainBuf strings.Builder
	offsets := make([]int, len(segs)+1)
	for i, s := range segs {
		offsets[i] = plainBuf.Len()
		plainBuf.WriteString(s.text)
	}
	offsets[len(segs)] = plainBuf.Len()
	plain := []rune(plainBuf.String())

	rows := strings.Split(ansi.WordwrapWc(string(plain), limit, "/"), "\n")
	out := make([][]segment, 0, len(rows))
	pos := 0
	for _, row := range rows {
		// Map each row back onto its plain-text range: the wrapper preserves
		// plain bytes in order and only drops spaces at break points, so a
		// mismatch means a dropped space. start anchors on the first matched
		// rune so dropped boundary spaces never leak into the styled piece.
		start := -1
		for _, r := range row {
			for pos < len(plain) && plain[pos] != r {
				pos++
			}
			if pos < len(plain) {
				if start < 0 {
					start = pos
				}
				pos++
			}
		}
		if start < 0 {
			start = pos
		}
		end := pos
		pieces := make([]segment, 0, 2)
		for i, s := range segs {
			s0, s1 := offsets[i], offsets[i+1]
			if s1 <= start || s0 >= end {
				continue
			}
			lo, hi := s0, s1
			if lo < start {
				lo = start
			}
			if hi > end {
				hi = end
			}
			pieces = append(pieces, segment{text: string(plain[lo:hi]), style: s.style})
		}
		if len(pieces) == 0 {
			pieces = append(pieces, segment{})
		}
		out = append(out, pieces)
	}
	return out
}

func renderSegment(s segment) string {
	switch s.style {
	case segBold:
		return boldStyle.Render(s.text)
	case segCode:
		return byteStyle.Render(s.text)
	default:
		return s.text
	}
}

// renderRow renders a wrapped row: every segment through its own style
// (plain pieces unstyled).
func renderRow(segs []segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(renderSegment(s))
	}
	return b.String()
}

// renderFlatRow renders every segment of a row with one flat style (used
// for headings, whose inline markers are consumed but whose whole line
// reads as one bold purple unit).
func renderFlatRow(segs []segment, style lipgloss.Style) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(style.Render(s.text))
	}
	return b.String()
}

// entryRows lays parsed blocks out as one row-group per block, each row ≤
// inner cells (styles and prefixes applied). One group per block keeps the
// 20-row cap entry-granular: a partially-fitting entry is dropped whole,
// never cut mid-bullet.
func entryRows(blocks []block, inner int) [][]string {
	groups := make([][]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.kind {
		case blockBlank:
			groups = append(groups, []string{""})
		case blockHeading:
			var rows []string
			for _, r := range wrapSegments(b.segments, inner) {
				rows = append(rows, renderFlatRow(r, headingStyle))
			}
			groups = append(groups, rows)
		case blockBullet:
			budget := inner - 2 // "· " prefix on the first row
			groups = append(groups, prefixedRows(wrapSegments(b.segments, budget),
				bulletMarkStyle.Render("· "), "  "))
		case blockContinuation:
			groups = append(groups, prefixedRows(wrapSegments(b.segments, inner-2),
				"  ", "  "))
		default: // blockPlain
			var rows []string
			for _, r := range wrapSegments(b.segments, inner) {
				rows = append(rows, renderRow(r))
			}
			groups = append(groups, rows)
		}
	}
	return groups
}

// prefixedRows prepends first (first row) and cont (continuation rows)
// prefixes to already-wrapped rows; style overrides row styling when
// non-nil.
func prefixedRows(rows [][]segment, first, cont string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		prefix := cont
		if i == 0 {
			prefix = first
		}
		out[i] = prefix + renderRow(r)
	}
	return out
}

// --- assembly: 20-row cap, title, footer ---

// maxInnerRows caps the box body at 20 content rows: title + entries +
// footer (the blank separator rows are structural and counted with them).
const maxInnerRows = 20

// assembleRows builds the box's inner lines: title, blank, entry rows
// (capped), "…and N more entries" on overflow, blank, footer last.
func assembleRows(groups [][]string, version string, inner int) []string {
	title := headingStyle.Render("What's new in v" + version)
	footer := wrapSegments([]segment{
		{text: "full changelog: "},
		{text: changelogURL(version), style: segBold},
	}, inner)
	// The footer URL is bold (segBold → boldStyle); the design wants the
	// label muted and the URL in phaseStyle, so restyle the footer rows.
	footerRows := make([]string, len(footer))
	for i, r := range footer {
		var b strings.Builder
		for _, s := range r {
			if s.style == segBold {
				b.WriteString(phaseStyle.Render(s.text))
			} else {
				b.WriteString(unavailableStyle.Render(s.text))
			}
		}
		footerRows[i] = b.String()
	}

	// entries-only budget: title + blank + entries + blank + footer ≤ 20
	fitBudget := maxInnerRows - 3 - len(footerRows)
	if fitBudget < 1 {
		fitBudget = 1
	}
	total := 0
	for _, g := range groups {
		total += len(g)
	}
	budget, truncated := fitBudget, total > fitBudget
	if truncated {
		budget = fitBudget - 1 // reserve a row for "…and N more entries"
		if budget < 1 {
			budget = 1
		}
	}

	var shown [][]string
	dropped := 0
	used := 0
	for i, g := range groups {
		if used+len(g) <= budget {
			shown = append(shown, g)
			used += len(g) // every row counts against the cap, blank or not
			continue
		}
		// The entry does not fit whole. Show its leading rows up to the cap
		// boundary — a long merged-flow bullet (the exit-code policy entry is
		// ~16 rows) flows to the cap instead of vanishing and leaving the box
		// with only its heading — then stop; the overflow line accounts for
		// the remainder, this entry included.
		if rest := budget - used; rest > 0 {
			shown = append(shown, g[:rest])
		}
		dropped = countEntries(groups[i:])
		break
	}
	// trim trailing blank rows left by the cut
	for len(shown) > 0 && shown[len(shown)-1][0] == "" {
		shown = shown[:len(shown)-1]
	}

	rows := []string{title, ""}
	for _, g := range shown {
		rows = append(rows, g...)
	}
	if truncated {
		rows = append(rows, unavailableStyle.Render(fmt.Sprintf("…and %d more entries", dropped)))
	}
	rows = append(rows, "")
	rows = append(rows, footerRows...)
	return rows
}

func countEntries(groups [][]string) int {
	n := 0
	for _, g := range groups {
		if !(len(g) == 1 && g[0] == "") {
			n++
		}
	}
	return n
}

// --- box family ---

// notesBox replicates the gradientBox family (cmd/sfu/tui.go:745-791):
// rounded corners ╭╮╰╯, a per-cell LUV ramp across the horizontal borders
// with the endpoints in the corners, solid mid-tone verticals, 2 cells of
// side padding (inner = outer-6), and rows fitted to exactly inner cells
// with tuiframe.PadOrTrim. No shared box renderer exists — gradientBox is
// triplicated per cmd — so this is a deliberate fourth instance of the same
// family, parameterized on the ramp colors.
func notesBox(groups [][]string, version string, outerWidth int, start, end colorful.Color) string {
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

	lines := assembleRows(groups, version, inner)
	out := make([]string, 0, len(lines)+2)
	out = append(out, strings.Repeat(" ", leftPad)+buildBorder("╭", "╮"))
	for _, ln := range lines {
		out = append(out, strings.Repeat(" ", leftPad)+midStyle.Render("│")+"  "+tuiframe.PadOrTrim(ln, inner)+"  "+midStyle.Render("│"))
	}
	out = append(out, strings.Repeat(" ", leftPad)+buildBorder("╰", "╯"))
	return strings.Join(out, "\n")
}

// plainNotes is the narrow (<40 outer) fallback: title, then the plain
// wrapped body (styles intact), no ramps — trading chrome for legibility.
func plainNotes(groups [][]string, version string, outer int) string {
	lines := assembleRows(groups, version, innerWidthFor(outer))
	for i, ln := range lines {
		lines[i] = strings.Repeat(" ", leftPad) + ln
	}
	return strings.Join(lines, "\n")
}
