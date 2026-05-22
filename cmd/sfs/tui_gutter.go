package main

import (
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
)

func outputPathForSummary(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// -o path below COMPLETE frame, full path no trim
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
