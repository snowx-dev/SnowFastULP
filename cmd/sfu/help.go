package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/version"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// hand-rolled help (not flag.PrintDefaults) so we control placeholders,
// muted suffixes, and 80-col layout. args[] tables mirror flag.* in main()
// and are kept in sync by hand.

// -h/--help → stdout, parse-error usage → stderr. non-TTY flips lipgloss
// profile to Ascii so log files dont fill with ANSI escapes.
func printHelp(bin string, w io.Writer) {
	if w == nil {
		w = helpDefaultWriter()
	}
	if f, ok := w.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		prev := lipgloss.DefaultRenderer().ColorProfile()
		lipgloss.DefaultRenderer().SetColorProfile(termenv.Ascii)
		defer lipgloss.DefaultRenderer().SetColorProfile(prev)
	}
	fmt.Fprint(w, renderHelp(bin))
}

// swappable for tests
var helpDefaultWriter = func() io.Writer { return os.Stderr }

func renderHelp(bin string) string {
	type argDef struct{ flag, ph, desc string }

	primary := []argDef{
		{"-o", "DIR", "Write output files to this folder."},
		{"-od", "DIR", "Write and dedup against old compressed results in this folder; this also compresses output."},
		{"-zst", "", "Compress the output with zstd."},
		{"-del", "", "Delete input .txt files after a successful run."},
		{"-no-uri", "", "Save only host:login:password."},
		{"-no-tui", "", "Use plain text output instead of the live screen."},
	}
	nerds := []argDef{
		{"-workers", "N", "Set parser worker count."},
		{"-dedup", "N", "Set dedup worker count."},
		{"-buckets", "N", "Set the number of temp buckets."},
		{"-temp-dir", "PATH", "Store temp files in this folder."},
		{"-split-zst", "N", "Split compressed output every N unique lines."},
		{"-loose", "", "Accept more input formats, with less strict parsing."},
		{"-no-encoding-sniff", "", "Skip encoding checks and read files as UTF-8."},
	}
	devs := []argDef{
		{"-debug", "", "Write a debug log for this run."},
		{"-debug-reject", "", "Write rejected input lines to a debug file."},
	}

	// fixed flag col, computed off unstyled length (SGR escapes would skew)
	flagCol := 0
	for _, a := range append(append(append([]argDef{}, primary...), nerds...), devs...) {
		w := len(a.flag)
		if a.ph != "" {
			w += 1 + len(a.ph)
		}
		if w > flagCol {
			flagCol = w
		}
	}
	flagCol += 4

	var b strings.Builder

	b.WriteString(phaseStyle.Render("SnowFastULP") + " " + mutedStyle.Render(version.String) + "\n")
	b.WriteString("\nTurbo ULP filter & de-duper.\nFire-and-forget: defaults are auto-tuned.\n\n")

	b.WriteString(labelStyle.Render("Usage:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " " +
		byteStyle.Render("<input-file-or-dir>") + " " +
		mutedStyle.Render("[-o DIR]") + "\n")
	b.WriteString(mutedStyle.Render("    More flags below: Args for nerds, then Args for devs (-debug, -debug-reject).") + "\n")
	b.WriteString(mutedStyle.Render("    Optional config: "+config.DefaultPathHint()+" (override: -config, SNOWFAST_CONFIG)") + "\n\n")

	b.WriteString(labelStyle.Render("Examples:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./logins.txt\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./mydir/ -o ./out/\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./mydir/ -o ./cleaned/\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./mydir/ -zst -split-zst 0\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./mydir/ -zst -no-uri\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./mydir/ -od ./library/   " +
		mutedStyle.Render("# dedup against past archives in ./library/") + "\n\n")

	b.WriteString(labelStyle.Render("Args:") + "\n")
	for _, a := range primary {
		b.WriteString(renderArgLine(a.flag, a.ph, a.desc, flagCol, countStyle, byteStyle) + "\n")
	}

	// nerds + devs render muted so the eye lands on primary first
	b.WriteString("\n" + labelStyle.Render("Args (for nerds):") + "\n")
	for _, a := range nerds {
		b.WriteString(renderArgLine(a.flag, a.ph, a.desc, flagCol, mutedStyle, mutedStyle) + "\n")
	}

	b.WriteString("\n" + labelStyle.Render("Args (for devs):") + "\n")
	for _, a := range devs {
		b.WriteString(renderArgLine(a.flag, a.ph, a.desc, flagCol, mutedStyle, mutedStyle) + "\n")
	}

	return b.String()
}

// "    -flag PH    desc (suffix)". trailing parens always muted.
// padding off unstyled len so SGR doesnt skew alignment.
func renderArgLine(flagName, ph, desc string, flagCol int, flagStyle, phStyle lipgloss.Style) string {
	flagText := flagName
	styled := flagStyle.Render(flagName)
	if ph != "" {
		flagText += " " + ph
		styled += " " + phStyle.Render(ph)
	}
	main, suffix := cliargs.SplitTrailingParen(desc)
	line := "    " + styled + strings.Repeat(" ", flagCol-len(flagText)) + main
	if suffix != "" {
		line += mutedStyle.Render(suffix)
	}
	return line
}
