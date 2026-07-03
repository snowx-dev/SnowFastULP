package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/termenv"
)

// runSecretsTable streams every matching row into memory, then renders a
// bordered, color-coded table to stdout. The secrets DB read is effectively
// instant and the result set is bounded by -l, so buffering is fine; the table
// is the human view, the TSV path handles pipes/-o. Count goes to stderr so it
// never mixes with the table on stdout.
func runSecretsTable(dbPath string, opts secrets.QueryOpts) error {
	// The table renders to stdout, so its color profile must follow stdout's
	// TTY status — not stderr's, which applyStderrColorProfile may have
	// downgraded to Ascii (a piped stderr with a TTY stdout would otherwise
	// strip the table's color). Restore the prior profile on the way out.
	prev := lipgloss.DefaultRenderer().ColorProfile()
	lipgloss.DefaultRenderer().SetColorProfile(secretsProfile(stdoutIsTTY()))
	defer lipgloss.DefaultRenderer().SetColorProfile(prev)

	var matches []secrets.Match
	n, err := secrets.QueryDB(dbPath, opts, func(m secrets.Match) error {
		matches = append(matches, m)
		return nil
	})
	if err != nil {
		return err
	}
	if n > 0 {
		fmt.Fprintln(os.Stdout, renderSecretsTable(matches, stdoutWidth()))
	}
	printSecretsSummary(n, "", true)
	return nil
}

// stdoutWidth is the terminal width measured against stdout (the table's sink),
// falling back to the shared display width when stdout isn't a real TTY — at
// which point the caller has already routed to the TSV path anyway.
func stdoutWidth() int {
	w, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil || w <= 0 {
		return tuiDisplayWidth
	}
	return w
}

var (
	secTypeStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})
	secSecretStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "234", Dark: "188"})
	secSourceStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
	secTimeStyle    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "162", Dark: "213"})
	secHeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
	secBorderStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "248", Dark: "238"})
	secHighStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "203"})
	secMedStyle     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	secLowStyle     = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
)

// Per-column caps keep one pathological cell (a 2 KiB JWT, a deep source path)
// from forcing the table resizer to crop unrelated columns mid-token; the
// resizer still shrinks to fit the actual terminal width after these caps.
const (
	secTypeCap   = 30
	secSecretCap = 80
	secSourceCap = 60
)

func severityStyle(sev string) lipgloss.Style {
	switch strings.ToLower(strings.TrimSpace(sev)) {
	case "high", "critical", "very high":
		return secHighStyle
	case "medium", "moderate":
		return secMedStyle
	default:
		return secLowStyle
	}
}

// renderSecretsTable lays out matches as a rounded-border table sized to width.
// StyleFunc indexes into matches so Severity can be colored by value, which the
// (row, col) callback alone can't recover.
func renderSecretsTable(matches []secrets.Match, width int) string {
	t := table.New().
		Headers("Type", "Secret", "Severity", "Source", "Last seen").
		BorderStyle(secBorderStyle).
		BorderTop(true).BorderBottom(true).BorderHeader(true).BorderColumn(true).
		Width(width).Wrap(false)

	for i := range matches {
		m := &matches[i]
		t.Row(
			trimToDisplayWidth(m.RuleName, secTypeCap),
			trimToDisplayWidth(m.Secret, secSecretCap),
			m.Severity,
			trimToDisplayWidth(sourceTail(m.SourcePath), secSourceCap),
			m.LastSeen.Format("2006-01-02 15:04"),
		)
	}

	t.StyleFunc(func(row, col int) lipgloss.Style {
		if row == table.HeaderRow {
			return secHeaderStyle
		}
		switch col {
		case 0:
			return secTypeStyle
		case 1:
			return secSecretStyle
		case 2:
			if row >= 0 && row < len(matches) {
				return severityStyle(matches[row].Severity)
			}
			return secLowStyle
		case 3:
			return secSourceStyle
		case 4:
			return secTimeStyle
		}
		return lipgloss.NewStyle()
	})

	return t.Render()
}

// sourceTail keeps the right-hand "where it came from" detail an analyst scans
// for: the inner path after a `archive!inner/path` split, or the path itself
// when there's no embedded member. Leading dirs are dropped only when an inner
// member exists, since a bare file path is already as specific as it gets.
func sourceTail(path string) string {
	if path == "" {
		return ""
	}
	if idx := strings.LastIndex(path, "!"); idx >= 0 && idx+1 < len(path) {
		return path[idx+1:]
	}
	return path
}

// secretsProfile picks the color profile for the secrets table render. The
// table goes to stdout, so it follows stdout's TTY status, not stderr's (the
// rest of the TUI targets stderr and applyStderrColorProfile downgrades that).
// A non-TTY stdout gets Ascii so ANSI escapes never leak into a pipe; a TTY
// stdout gets its actually-detected profile, undoing any stderr-driven
// downgrade for the duration of the render.
func secretsProfile(stdoutTTY bool) termenv.Profile {
	if !stdoutTTY {
		return termenv.Ascii
	}
	return termenv.NewOutput(os.Stdout).Profile
}
