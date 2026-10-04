package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/cliargs"
	"github.com/snowx-dev/SnowFastULP/internal/config"
	"github.com/snowx-dev/SnowFastULP/internal/version"
	"golang.org/x/term"
)

// stdout for -h/--help, stderr for flag.Usage parse errors
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

var helpDefaultWriter = func() io.Writer { return os.Stderr }

func renderHelp(bin string) string {
	type argDef struct{ flag, ph, desc string }

	primary := []argDef{
		{"-txt", "", "Search plain .txt files instead of .zst archives (no index)."},
		{"-o", "FILE", "Write results to FILE. With -f: per-term DIR"},
		{"-stats", "", "Live progress screen; write hits to an auto CWD result file (or -o)."},
		{"-clean", "", "Strip URL schemes from output lines."},
		{"-combo", "", "Write only login:password per hit; lines that are not U:L:P are skipped."},
		{"-l", "N", "Stop after N matching lines, then exit (0 = unlimited)."},
		{"-since", "DUR", "Only search archives modified within DUR, e.g. 7d, 12h, 90m."},
		{"-f", "FILE", "Search every term in FILE (one per line) in a single pass. With -o DIR, write one file per term."},
		{"-bell", "", "Play a short sound when the run finishes."},
	}
	nerds := []argDef{
		{"-workers", "N", "Set search worker count."},
		{"-json", "", "Stream live stats as NDJSON (stdout, or a file with -json FILE). Owns output: hits require -o FILE."},
		{"-json-every", "DURATION", "Update interval for -json (default 800ms, minimum 200ms)."},
	}
	devs := []argDef{
		{"-debug", "", "Write a debug log for this run."},
		{"-no-update-check", "", "Disable background update availability check."},
		{"-s", "", "Deprecated, use default."},
		{"-silent", "", "Deprecated, use default."},
		{"-j", "N", "Alias for -workers."},
		{"-decode-step", "BYTES", "Per-Read decode budget (default 1048576)."},
		{"-max-hits-per-chunk", "N", "Truncate matching lines per chunk to N (default 0 = unbounded)."},
	}

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

	b.WriteString(phaseStyle.Render("SnowFastSearch") + " " + mutedStyle.Render(version.String) + "\n")
	b.WriteString("\nParallel search over .zst archives (index-backed) or plain .txt files (-txt).\n\n")

	b.WriteString(labelStyle.Render("Usage:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " " +
		byteStyle.Render("PATTERN") + " " +
		mutedStyle.Render("[-o FILE] [-stats] [-f FILE]") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " " +
		byteStyle.Render("DIR") + " " +
		byteStyle.Render("PATTERN") + " " +
		mutedStyle.Render("[-o FILE] [-stats] [-f FILE]") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " " +
		mutedStyle.Render("-f ") + byteStyle.Render("TERMS") + " " +
		mutedStyle.Render("[-o DIR]") + "\n")
	b.WriteString(mutedStyle.Render("    Flags may appear before or after the pattern.") + "\n")
	b.WriteString(mutedStyle.Render("    Optional config: "+config.DefaultPathHint()) + "\n")
	b.WriteString(mutedStyle.Render("    PATTERN '*' exports every line (quote it in the shell).") + "\n")
	b.WriteString(mutedStyle.Render("    -f FILE: one search term per line; '*' is not allowed in file mode.") + "\n\n")

	b.WriteString(labelStyle.Render("Commands:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " update   " +
		mutedStyle.Render("# upgrade sfu, sfs & sfl to the latest release; --dry-run previews") + "\n\n")

	b.WriteString(labelStyle.Render("Examples:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " 'facebook.com:'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " -txt ./dumps 'user@example'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library 'user@example'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library 'gmail' | head\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library '*' -since 5m -o recent.txt\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library -stats 'user@example'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library -stats -o hits.txt 'user@example'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " 'pattern' -o out.txt -clean\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " -f terms.txt ./library\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " -f terms.txt ./library -o results/\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library 'pattern' -debug\n\n")

	b.WriteString(labelStyle.Render("Args:") + "\n")
	for _, a := range primary {
		b.WriteString(renderHelpArgLine(a.flag, a.ph, a.desc, flagCol, countStyle, byteStyle) + "\n")
	}

	b.WriteString("\n" + labelStyle.Render("Args (for nerds):") + "\n")
	for _, a := range nerds {
		b.WriteString(renderHelpArgLine(a.flag, a.ph, a.desc, flagCol, mutedStyle, mutedStyle) + "\n")
	}

	b.WriteString("\n" + labelStyle.Render("Args (for devs):") + "\n")
	for _, a := range devs {
		b.WriteString(renderHelpArgLine(a.flag, a.ph, a.desc, flagCol, mutedStyle, mutedStyle) + "\n")
	}
	return b.String()
}

func renderHelpArgLine(flagName, ph, desc string, flagCol int, flagStyle, phStyle lipgloss.Style) string {
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
