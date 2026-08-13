package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
	"github.com/snowx-dev/SnowFastULP/internal/pathdisp"
)

func outputPathForSummary(path string) string {
	return pathdisp.ForDisplay(path)
}

// -o path below COMPLETE frame, prefer CWD-relative form
func renderOutputFooter(outFile string, boxStart, boxEnd colorful.Color) []string {
	outFile = outputPathForSummary(outFile)
	if outFile == "" {
		return nil
	}
	mid := boxStart.BlendLuv(boxEnd, 0.5)
	border := lipgloss.NewStyle().Foreground(lipgloss.Color(mid.Hex()))
	labelCell := labelStyle.Render("Output   ")
	prefix := gutterPrefix(border)
	return []string{"", prefix + labelCell + phaseStyle.Render(outFile)}
}

func gutterPrefix(border lipgloss.Style) string {
	return strings.Repeat(" ", leftPad) + border.Render("┃") + "  "
}
