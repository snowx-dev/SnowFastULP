package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/x/term"
)

// runSecretsTable streams every matching row into memory, then renders a
// bordered, color-coded table to stdout. The secrets DB read is effectively
// instant and the result set is bounded by -l, so buffering is fine; the table
// is the human view, the TSV path handles pipes/-o. Count goes to stderr so it
// never mixes with the table on stdout.
func runSecretsTable(dbPath string, opts secrets.QueryOpts) error {
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
	if n > 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
		if _, err := offerSecretsExport(matches, cwd, os.Stdin, os.Stderr); err != nil {
			return err
		}
	}
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
	secTypeStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "29", Dark: "82"})
	secSecretStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "234", Dark: "188"})
	secSourceStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "240"})
	secTimeStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "162", Dark: "213"})
	secHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
	secBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "248", Dark: "238"})
)

// Per-column caps keep one pathological cell (a 2 KiB JWT, a deep source path)
// from forcing the table resizer to crop unrelated columns mid-token; the
// resizer still shrinks to fit the actual terminal width after these caps.
const (
	secTypeCap   = 30
	secSecretCap = 80
	secSourceCap = 60
)

// renderSecretsTable lays out matches as a rounded-border table sized to width.
func renderSecretsTable(matches []secrets.Match, width int) string {
	t := table.New().
		Headers("Type", "Secret", "Source", "Last seen").
		BorderStyle(secBorderStyle).
		BorderTop(true).BorderBottom(true).BorderHeader(true).BorderColumn(true).
		Width(width).Wrap(false)

	for i := range matches {
		m := &matches[i]
		t.Row(
			trimToDisplayWidth(m.RuleName, secTypeCap),
			trimToDisplayWidth(m.Secret, secSecretCap),
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
			return secSourceStyle
		case 3:
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

// offerSecretsExport asks on out (stderr) whether to dump the matched secrets to
// a clean, one-secret-per-line txt file in cwd. Default is no: an empty or
// unrecognized reply writes nothing. On yes it dedups the secret values and
// writes them to sfs_secrets_<stamp>.txt, returning the path. in/out are
// parameters so the prompt is testable without touching real stdio.
func offerSecretsExport(matches []secrets.Match, cwd string, in io.Reader, out io.Writer) (string, error) {
	if len(matches) == 0 {
		return "", nil
	}
	fmt.Fprintf(out, "Export %d secret(s) to a clean txt in %s? [y/N] ", len(matches), cwd)
	reader := bufio.NewReader(in)
	line, _ := reader.ReadString('\n')
	if !confirmYes(strings.TrimSpace(line)) {
		fmt.Fprintln(out, "skipped")
		return "", nil
	}
	path, err := defaultSecretsExportPath(cwd, time.Now())
	if err != nil {
		return "", err
	}
	seen := make(map[string]struct{}, len(matches))
	var buf strings.Builder
	for _, m := range matches {
		if m.Secret == "" {
			continue
		}
		if _, dup := seen[m.Secret]; dup {
			continue
		}
		seen[m.Secret] = struct{}{}
		buf.WriteString(m.Secret)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		return "", fmt.Errorf("write export: %w", err)
	}
	noun := "secrets"
	if len(seen) == 1 {
		noun = "secret"
	}
	fmt.Fprintf(out, "exported %d %s → %s\n", len(seen), noun, path)
	return path, nil
}

func confirmYes(s string) bool {
	switch strings.ToLower(s) {
	case "y", "yes":
		return true
	}
	return false
}
