package releasenotes

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// forceDark pins adaptive-color resolution to dark for deterministic golden
// output, restored on cleanup.
func forceDark(t *testing.T) {
	t.Helper()
	prev := lipgloss.DefaultRenderer().HasDarkBackground()
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(prev) })
}

func seg(text string, style segStyle) segment { return segment{text: text, style: style} }

// TestParseInlineDegradeRows pins the §D2 parser table: every supported
// construct renders as specified, and unbalanced markers degrade to plain
// with the literal markers kept.
func TestParseInlineDegradeRows(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []segment
	}{
		{"plain", "just text", []segment{seg("just text", segPlain)}},
		{"bold", "**very bold**", []segment{seg("very bold", segBold)}},
		{"code", "`run -h`", []segment{seg("run -h", segCode)}},
		{"mixed", "see `sfu -h` and **stop** now",
			[]segment{seg("see ", segPlain), seg("sfu -h", segCode),
				seg(" and ", segPlain), seg("stop", segBold), seg(" now", segPlain)}},
		{"unbalanced bold keeps markers", "**oops", []segment{seg("**oops", segPlain)}},
		{"unbalanced code keeps markers", "tic`ket", []segment{seg("tic`ket", segPlain)}},
		{"code with unbalanced bold degrades", "`a**b`", []segment{seg("`a**b`", segPlain)}},
		{"empty", "", []segment{seg("", segPlain)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseInline(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseInline(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseInline(%q)[%d] = %+v, want %+v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseBlocksConstructs(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		kinds []blockKind
		texts []string // flattened segment texts per block, "-" separated
	}{
		{"heading", "### Changed\n- item", []blockKind{blockHeading, blockBullet},
			[]string{"Changed", "item"}},
		{"top-level titles skipped", "# Title\n## Section\n### Kept\n", []blockKind{blockHeading},
			[]string{"Kept"}},
		{"bullet continuations merge into one flow", "- first\n  continued\n- second",
			[]blockKind{blockBullet, blockBullet},
			[]string{"first continued", "second"}},
		{"numbered stays plain", "1. item\n2. item", []blockKind{blockPlain, blockPlain},
			[]string{"1. item", "2. item"}},
		{"blank collapses", "- a\n\n\n\n- b", []blockKind{blockBullet, blockBlank, blockBullet},
			[]string{"a", "", "b"}},
		{"links reduced to text", "see [docs](https://example.com) here",
			[]blockKind{blockPlain}, []string{"see docs here"}},
		{"html tags stripped", "line <b>bold</b> end", []blockKind{blockPlain},
			[]string{"line bold end"}},
		{"placeholders survive", "uses <version> and <tool> markers", []blockKind{blockPlain},
			[]string{"uses <version> and <tool> markers"}},
		{"indented fence inside bullet flows with markers kept",
			"- has code:\n  ```sh\n  run --now\n  ```\n- next",
			[]blockKind{blockBullet, blockBullet},
			[]string{"has code: -sh run --now ", "next"}}, // "-" = test join separator
		{"top-level fence degrade unchanged", "```\nplain fenced\n```\n- after",
			[]blockKind{blockPlain, blockBullet},
			[]string{"plain fenced", "after"}},
		{"leading trailing blanks trimmed", "\n\n- only\n\n", []blockKind{blockBullet},
			[]string{"only"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := parseBlocks(tc.in)
			if len(blocks) != len(tc.kinds) {
				t.Fatalf("got %d blocks %+v, want %d", len(blocks), blocks, len(tc.kinds))
			}
			for i, b := range blocks {
				if b.kind != tc.kinds[i] {
					t.Errorf("block %d kind = %d, want %d (%+v)", i, b.kind, tc.kinds[i], b)
				}
				var parts []string
				for _, s := range b.segments {
					parts = append(parts, s.text)
				}
				if got := strings.Join(parts, "-"); got != tc.texts[i] {
					t.Errorf("block %d text = %q, want %q", i, got, tc.texts[i])
				}
			}
		})
	}
}

// Wide runes count 2 cells and are never split across a wrap boundary.
func TestWrapSegmentsWideCJK(t *testing.T) {
	// Two "漢字" words (4 cells each) fit on one 9-cell row, wrap at 8.
	segs := []segment{{text: "漢字 漢字"}}
	one := wrapSegments(segs, 9)
	if len(one) != 1 || ansi.StringWidth(renderRow(one[0])) != 9 {
		t.Fatalf("limit 9: got %d rows (%+v), want one 9-cell row", len(one), one)
	}
	two := wrapSegments(segs, 8)
	if len(two) != 2 {
		t.Fatalf("limit 8: got %d rows, want 2", len(two))
	}
	for i, r := range two {
		if w := ansi.StringWidth(renderRow(r)); w != 4 {
			t.Errorf("limit 8 row %d width = %d, want 4", i, w)
		}
	}
}

// A styled segment split across a wrap column re-opens its SGR on the
// continuation piece.
func TestWrapSegmentsReopensSGRAcrossWrap(t *testing.T) {
	forceTrueColor(t)
	segs := []segment{
		{text: "intro "},
		{text: "boldpart one two three", style: segBold},
		{text: " tail"},
	}
	rows := wrapSegments(segs, 10)
	rendered := make([]string, len(rows))
	for i, r := range rows {
		rendered[i] = renderRow(r)
	}
	find := func(substr string) string {
		for _, r := range rendered {
			if strings.Contains(r, substr) {
				return r
			}
		}
		return ""
	}
	// "boldpart" starts the bold region; "one"/"two" continue it on later
	// rows and must each carry their own SGR 1.
	for _, piece := range []string{"boldpart", "one", "two"} {
		row := find(piece)
		if row == "" {
			t.Fatalf("no rendered row contains %q: %q", piece, strings.Join(rendered, "|"))
		}
		if !strings.Contains(row, "\x1b[1m") {
			t.Errorf("row containing %q does not re-open bold SGR: %q", piece, row)
		}
	}
	// "intro" is plain: no bold SGR on its row.
	if row := find("intro"); strings.Contains(row, "\x1b[1m") {
		t.Errorf("plain row carries bold SGR: %q", row)
	}
}

// The box body caps at 20 inner rows; overflow inserts the muted
// "…and N more entries" row and keeps the footer last.
func TestBoxRowCapAndFooterLast(t *testing.T) {
	forceAscii(t)
	var b strings.Builder
	b.WriteString("### Heading\n\n")
	for i := 1; i <= 30; i++ {
		b.WriteString("- entry ")
		b.WriteString(strconv.Itoa(i))
		b.WriteString("\n")
	}
	var buf bytes.Buffer
	if err := Render(&buf, b.String(), "0.3.0", 80, notesGradStart, notesGradEnd); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := strings.TrimRight(buf.String(), "\n")
	lines := strings.Split(out, "\n")
	// 2 borders + 20 inner rows
	if len(lines) != 22 {
		t.Fatalf("got %d lines, want 22 (2 borders + 20 capped inner rows):\n%s", len(lines), out)
	}
	// 12 entries fit the 14-row budget (heading + blank + 12 bullets), so
	// 18 of the 30 are deferred to the overflow line.
	if want := "…and 18 more entries"; !strings.Contains(out, want) {
		t.Errorf("missing overflow line %q:\n%s", want, out)
	}
	if strings.Contains(out, "entry 13") || strings.Contains(out, "entry 30") {
		t.Errorf("entries beyond the cap leaked:\n%s", out)
	}
	if !strings.Contains(out, "entry 12") {
		t.Errorf("entry 12 should be shown:\n%s", out)
	}
	if !strings.Contains(lines[len(lines)-1], "╯") {
		t.Errorf("last line must close the box:\n%s", lines[len(lines)-1])
	}
	if !strings.Contains(lines[len(lines)-2], "CHANGELOG.md") {
		t.Errorf("footer must be the last inner row:\n%s", lines[len(lines)-2])
	}
	// more-line sits directly above the blank before the footer
	if !strings.Contains(lines[17], "more entries") || strings.ReplaceAll(lines[18], " ", "") != "││" ||
		!strings.Contains(strings.ReplaceAll(lines[19], " ", ""), "fullchangelog:") {
		t.Errorf("unexpected tail rows:\n%s", strings.Join(lines[14:], "\n"))
	}
}

// Narrow terminals: below the 40-cell outer threshold the box loses its
// borders entirely, styles intact.
func TestRenderNarrowFallsBackToBorderless(t *testing.T) {
	forceAscii(t)
	notes := "### Changed\n- something new\n"
	var buf bytes.Buffer
	if err := Render(&buf, notes, "0.3.0", 47, notesGradStart, notesGradEnd); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	for _, bad := range []string{"╭", "╮", "╰", "╯", "│", "─"} {
		if strings.Contains(out, bad) {
			t.Errorf("narrow output contains border rune %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"What's new in v0.3.0", "something new", "full changelog:"} {
		if !strings.Contains(out, want) {
			t.Errorf("narrow output missing %q:\n%s", want, out)
		}
	}
}

// Golden box render at width 80 under truecolor: the ramp hexes #5A56E0 →
// #AE61EA → #EE6FF8 appear, the title is bold purple, inline code is amber,
// and every box line is exactly outer (72) cells wide.
func TestGoldenBoxRenderAtWidth80TrueColor(t *testing.T) {
	forceTrueColor(t)
	forceDark(t)
	notes := "### Changed — exit codes are now failure-aware (behavior change for scripts)\n" +
		"- **The tools no longer exit 0 on failed runs.** All three binaries now follow\n" +
		"  one documented policy (see each `-h`): `0` clean run, `1` runtime error,\n" +
		"  `2` usage error, `3` partial results (sfl: some sources failed or partial\n" +
		"  results were still written).\n"
	var buf bytes.Buffer
	if err := Render(&buf, notes, "0.3.0", 80, notesGradStart, notesGradEnd); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()

	// The design hexes must flow through the render pipeline. Expected SGR
	// sequences are derived through the same lipgloss/termenv path (termenv
	// truncates colorful RGB255 values, e.g. #5A56E0 → 38;2;89;86;224), so
	// the assertion pins the hex → SGR mapping, not float rounding.
	fgSeq := func(c lipgloss.TerminalColor) string {
		s := lipgloss.NewStyle().Foreground(c).Render("X")
		i := strings.Index(s, "\x1b[")
		return s[i+2 : strings.Index(s[i:], "m")+i]
	}
	startSeq := fgSeq(lipgloss.Color(notesGradStart.Hex()))
	midSeq := fgSeq(lipgloss.Color(notesGradStart.BlendLuv(notesGradEnd, 0.5).Hex()))
	endSeq := fgSeq(lipgloss.Color(notesGradEnd.Hex()))
	amberSeq := fgSeq(lipgloss.AdaptiveColor{Light: "178", Dark: "222"})
	mutedSeq := fgSeq(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
	phaseSeq := fgSeq(lipgloss.AdaptiveColor{Light: "33", Dark: "51"})
	for _, tc := range []struct{ seq, what string }{
		{startSeq, "ramp start #5A56E0"},
		{midSeq, "mid verticals #AE61EA"},
		{endSeq, "ramp end #EE6FF8"},
		{"1;" + startSeq, "bold purple title"},
		{amberSeq, "amber code span"},
		{mutedSeq, "muted bullet marker"},
		{"1;" + phaseSeq, "bold phase footer URL"},
	} {
		if !strings.Contains(out, "\x1b["+tc.seq+"m") {
			t.Errorf("output missing %s SGR \\x1b[%sm:\n%s", tc.what, tc.seq, out)
		}
	}
	if !strings.Contains(out, "What's new in v0.3.0") {
		t.Errorf("output missing title:\n%s", out)
	}

	// geometry: outer 80 - 8 indent = 72, inner 66; every line is the 72-cell
	// box row plus the 4-column indent
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("box too small:\n%s", out)
	}
	for i, line := range lines {
		if w := ansi.StringWidth(line); w != 76 {
			t.Errorf("line %d visible width = %d, want 76: %q", i, w, line)
		}
	}
}

// A bullet whose source is hard-wrapped across 3 lines renders as ONE
// flowed paragraph: greedy wrap fills every row to the full budget, so no
// ragged orphan rows appear mid-bullet, and continuation rows keep their
// 2-cell alignment under the "· " marker.
func TestBulletContinuationLinesMergeIntoOneFlow(t *testing.T) {
	forceAscii(t)
	notes := "- alpha beta gamma delta epsilon zeta eta theta iota kappa\n" +
		"  lambda mu nu xi omicron pi rho sigma tau upsilon phi chi\n" +
		"  psi omega end of the merged paragraph text\n"
	var buf bytes.Buffer
	if err := Render(&buf, notes, "0.3.0", 80, notesGradStart, notesGradEnd); err != nil {
		t.Fatalf("Render: %v", err)
	}
	var rows []string
	for _, ln := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		stripped := ansi.Strip(ln)
		// bullet rows: "    │  · …" (first) or "    │    …" (continuation);
		// blank inner rows share the shapes but carry no content
		body := strings.TrimSpace(strings.TrimSuffix(
			strings.TrimPrefix(stripped, "    │"), "│"))
		if (strings.HasPrefix(stripped, "    │  ·") || strings.HasPrefix(stripped, "    │    ")) && body != "" {
			rows = append(rows, stripped)
		}
	}
	if len(rows) < 2 {
		t.Fatalf("merged bullet should wrap to >=2 rows, got %d:\n%s", len(rows), buf.String())
	}
	// merged text = "alpha … theta iota kappa lambda mu nu … psi omega end of
	// the merged paragraph text"; greedy wrap at the 64-cell budget (inner
	// 66 minus the 2-cell prefix) yields exactly 3 rows: 63 + 63 cells (the
	// next word never fits) + a short tail — the pre-wrap source's 46/41/34
	// orphan rows are gone.
	if len(rows) != 3 {
		t.Fatalf("merged bullet should wrap to exactly 3 flowed rows, got %d:\n%s", len(rows), buf.String())
	}
	for i, row := range rows {
		// row = "    │" + 2-cell prefix ("· " or "  ") + wrapped text + pad + "│"
		content := strings.TrimRight(strings.TrimSuffix(
			strings.TrimPrefix(row, "    │"), "│"), " ")
		wrapped := ansi.StringWidth(content) - 2
		if i < len(rows)-1 && wrapped < 63 {
			t.Errorf("mid-bullet row %d is an orphan: %d cells: %q", i, wrapped, row)
		}
		if i > 0 && !strings.HasPrefix(row, "    │    ") {
			t.Errorf("continuation row %d not aligned 2 cells under the marker: %q", i, row)
		}
	}
	if strings.Contains(rows[0], "mu nu") || !strings.Contains(rows[1], "mu nu") {
		t.Errorf("flow split is not the greedy maximum:\n%s", buf.String())
	}
	// the merged sentence flows across row boundaries (and past the box
	// borders), so assert on border-stripped, whitespace-normalized text
	flat := strings.Join(strings.Fields(
		strings.Map(func(r rune) rune {
			if strings.ContainsRune("│─╭╮╰╯", r) {
				return -1
			}
			return r
		}, ansi.Strip(buf.String()))), " ")
	for _, want := range []string{"alpha beta gamma delta", "psi omega end of the merged paragraph text"} {
		if !strings.Contains(flat, want) {
			t.Errorf("merged flow lost text %q:\n%s", want, buf.String())
		}
	}
}

// An entry that cannot fit whole under the 20-row cap (a long merged-flow
// bullet) shows its leading rows up to the cap boundary instead of
// vanishing and leaving the box with only its heading; the overflow line
// still follows and the footer stays last.
func TestRowCapShowsLeadingRowsOfOversizedEntry(t *testing.T) {
	forceAscii(t)
	notes := "### Heading\n\n- " + strings.Repeat("word ", 200) + "end\n" +
		"- a later entry that must not appear\n"
	var buf bytes.Buffer
	if err := Render(&buf, notes, "0.3.0", 80, notesGradStart, notesGradEnd); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := strings.TrimRight(buf.String(), "\n")
	lines := strings.Split(out, "\n")
	if len(lines) != 22 {
		t.Fatalf("got %d lines, want 22 (2 borders + 20 capped inner rows):\n%s", len(lines), out)
	}
	if !strings.Contains(out, "…and 2 more entries") { // the partial bullet + the later entry
		t.Errorf("missing overflow line:\n%s", out)
	}
	if strings.Contains(out, "a later entry") {
		t.Errorf("deferred entry leaked:\n%s", out)
	}
	if !strings.Contains(lines[len(lines)-2], "CHANGELOG.md") {
		t.Errorf("footer must be last inner row:\n%s", lines[len(lines)-2])
	}
	// every shown word-bullet row except the last fills the 66-cell budget
	bulletRows := 0
	for _, ln := range lines {
		stripped := ansi.Strip(ln)
		body := strings.TrimRight(strings.TrimSuffix(strings.TrimPrefix(stripped, "    │"), "│"), " ")
		if !strings.Contains(body, "word") {
			continue
		}
		bulletRows++
		if !strings.Contains(body, "end") && ansi.StringWidth(body) != 68 {
			t.Errorf("shown bullet row not filled to inner budget (68 = 2 pad + 66): %d cells: %q",
				ansi.StringWidth(body), stripped)
		}
	}
	if bulletRows == 0 {
		t.Fatalf("no leading bullet rows shown:\n%s", out)
	}
}
