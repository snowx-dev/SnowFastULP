package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/snowx-dev/SnowFastULP/internal/version"
	"golang.org/x/term"
)

// -h/--help → stdout, parse-error usage → stderr. Non-TTY flips the lipgloss
// profile to Ascii so redirected help/log files don't fill with ANSI escapes.
func printHelp(bin string, w io.Writer) {
	if w == nil {
		w = os.Stderr
	}
	if f, ok := w.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		prev := lipgloss.DefaultRenderer().ColorProfile()
		lipgloss.DefaultRenderer().SetColorProfile(termenv.Ascii)
		defer lipgloss.DefaultRenderer().SetColorProfile(prev)
	}
	fmt.Fprint(w, renderHelp(bin))
}

func renderHelp(bin string) string {
	type argDef struct{ flag, ph, desc string }
	primary := []argDef{
		{"-o", "DIR", "Write extracted ULP lines to this folder."},
		{"-od", "DIR", "Ingest extracted ULP lines into an existing sfu library."},
		{"-p", "PASSWORD_OR_FILE", "Archive password or password-list file."},
		{"-zst", "", "Compress classic output with zstd."},
		{"-no-uri", "", "Save only host:login:password."},
		{"-no-tui", "", "Use plain text output instead of the live screen."},
	}
	nerds := []argDef{
		{"-workers", "N", "Set parser/archive worker count."},
		{"-temp-dir", "PATH", "Store temp files in this folder."},
		{"-del", "", "Delete source archives/files after a successful run."},
	}
	devs := []argDef{
		{"-debug", "", "Write a debug log for this run."},
		{"-err", "", "Write the full, untruncated issue list to a file."},
		{"-no-update-check", "", "Disable background update availability check."},
	}

	// fixed flag col, computed off unstyled length (SGR escapes would skew it)
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
	b.WriteString(sflTitleStyle.Render("SnowFastLog") + " " + sflMutedStyle.Render(version.String) + "\n")
	b.WriteString("\nExtract stealer logs into clean ULP lines.\n\n")

	b.WriteString(sflLabelStyle.Render("Usage:") + "\n")
	b.WriteString("  " + sflOkStyle.Render(bin) + " " + sflWarnStyle.Render("INPUT_PATH") + " " +
		sflMutedStyle.Render("-o ./ulp/") + "\n")
	b.WriteString("  " + sflOkStyle.Render(bin) + " " + sflWarnStyle.Render("INPUT_PATH") + " " +
		sflMutedStyle.Render("-od ./library/ -p passwords.txt") + "\n\n")

	b.WriteString(sflLabelStyle.Render("Examples:") + "\n")
	b.WriteString("  " + sflOkStyle.Render(bin) + " ./extracted-log/ -o ./ulp/\n")
	b.WriteString("  " + sflOkStyle.Render(bin) + " ./archives/ -od ./library/ -p common-passwords.txt\n\n")

	appendSection := func(title string, args []argDef, flagStyle, phStyle lipgloss.Style) {
		b.WriteString(sflLabelStyle.Render(title+":") + "\n")
		for _, a := range args {
			b.WriteString(renderArgLine(a.flag, a.ph, a.desc, flagCol, flagStyle, phStyle) + "\n")
		}
		b.WriteString("\n")
	}
	// primary stands out; nerds/devs render muted so the eye lands on primary.
	appendSection("Args", primary, sflOkStyle, sflWarnStyle)
	appendSection("Args (for nerds)", nerds, sflMutedStyle, sflMutedStyle)
	appendSection("Args (for devs)", devs, sflMutedStyle, sflMutedStyle)
	return b.String()
}

// "  -flag PH    desc". Padding is computed off the unstyled length so SGR
// escapes don't skew the flag column.
func renderArgLine(flagName, ph, desc string, flagCol int, flagStyle, phStyle lipgloss.Style) string {
	flagText := flagName
	styled := flagStyle.Render(flagName)
	if ph != "" {
		flagText += " " + ph
		styled += " " + phStyle.Render(ph)
	}
	line := "  " + styled
	if pad := flagCol - len(flagText); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return line + desc
}
