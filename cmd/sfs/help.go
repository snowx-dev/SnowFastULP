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
		{"-o", "FILE", "Write results to this file instead of the auto-generated CWD file."},
		{"-s", "", "Stream results to stdout without the live screen."},
		{"-clean", "", "Strip URL schemes from output lines."},
		{"-l", "N", "Stop after N total hits, then exit (0 = unlimited)."},
		{"-since", "DUR", "Only search archives modified within DUR, e.g. 7d, 12h, 90m."},
		{"-sec", "", "Search the secrets DB (from `sfl -secrets`); PATTERN filters by type ('*' = all)."},
		{"-secrets-path", "PATH", "Path to the secrets DB (default: <root>/sfl-secrets.sqlite). Implies -sec; -sec-path is an alias."},
	}
	nerds := []argDef{
		{"-j", "N", "Set search worker count."},
	}
	devs := []argDef{
		{"-debug", "", "Write a debug log for this run."},
		{"-no-update-check", "", "Disable background update availability check."},
		{"-silent", "", "Deprecated alias for -s."},
		{"-decode-step", "BYTES", "Per-Read decode budget (default 1048576)."},
		{"-max-hits-per-chunk", "N", "Truncate hits per chunk to N (default 0 = unbounded). Safety valve for `:` / `@` -style queries."},
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
	b.WriteString("\nParallel search over .zst archives (index-backed) or plain .txt files (-txt).\nIndexes are built automatically for .zst when needed.\n\n")

	b.WriteString(labelStyle.Render("Usage:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " " +
		byteStyle.Render("PATTERN") + " " +
		mutedStyle.Render("[-o FILE | -s]") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " " +
		byteStyle.Render("DIR") + " " +
		byteStyle.Render("PATTERN") + " " +
		mutedStyle.Render("[-o FILE | -s]") + "\n")
	b.WriteString(mutedStyle.Render("    Flags may appear before or after the pattern. More flags below: Args for nerds, then Args for devs.") + "\n")
	b.WriteString(mutedStyle.Render("    Optional config: "+config.DefaultPathHint()+" (override: -config, SNOWFAST_CONFIG; [sfs].dir for PATTERN-only)") + "\n")
	b.WriteString(mutedStyle.Render("    PATTERN '*' exports every line (quote it in the shell).") + "\n\n")

	b.WriteString(labelStyle.Render("Commands:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " update   " +
		mutedStyle.Render("# upgrade sfu, sfs & sfl to the latest release") + "\n\n")

	b.WriteString(labelStyle.Render("Examples:") + "\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " 'facebook.com:'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " -txt ./dumps 'user@example'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library 'user@example'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library 'gmail' -s | head\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library '*' -since 5m -o recent.txt\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " ./library -o hits.txt 'user@example'\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " 'pattern' -s\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " 'pattern' -o out.txt -clean\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " 'aws' -sec -since 1h\n")
	b.WriteString("    " + phaseStyle.Render(bin) + " '*' -sec -l 10\n")
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
