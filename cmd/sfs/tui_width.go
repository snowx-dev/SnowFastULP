package main

import (
	"os"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
)

const (
	tuiDisplayWidth = 86
	ansiReset       = "\033[0m"
)

func termWidth() int {
	w, _, err := term.GetSize(os.Stderr.Fd())
	if err != nil || w <= 0 {
		return tuiDisplayWidth
	}
	if w > tuiDisplayWidth {
		return tuiDisplayWidth
	}
	return w
}

func termHeight() int {
	_, h, err := term.GetSize(os.Stderr.Fd())
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

func tuiVisibleWidth(s string) int {
	b := []byte(s)
	i := 0
	n := 0
	for i < len(b) {
		if b[i] == '\033' && i+1 < len(b) && b[i+1] == '[' {
			i += 2
			for i < len(b) && b[i] != 'm' {
				i++
			}
			if i < len(b) {
				i++
			}
			continue
		}
		_, sz := utf8.DecodeRune(b[i:])
		if sz == 0 {
			i++
			continue
		}
		i += sz
		n++
	}
	return n
}

func trimToDisplayWidth(s string, max int) string {
	if tuiVisibleWidth(s) <= max {
		return s
	}
	var b strings.Builder
	v := 0
	bytes := []byte(s)
	i := 0
	for i < len(bytes) {
		if bytes[i] == '\033' && i+1 < len(bytes) && bytes[i+1] == '[' {
			j := i + 2
			for j < len(bytes) && bytes[j] != 'm' {
				j++
			}
			if j < len(bytes) {
				b.Write(bytes[i : j+1])
				i = j + 1
			} else {
				i = j
			}
			continue
		}
		if v >= max-1 {
			break
		}
		r, sz := utf8.DecodeRune(bytes[i:])
		if sz == 0 {
			i++
			continue
		}
		b.Write(bytes[i : i+sz])
		i += sz
		if r != utf8.RuneError {
			v++
		}
	}
	b.WriteString(ansiReset)
	b.WriteString("…")
	return b.String()
}

func trimLinesToWidth(lines []string, max int) []string {
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = trimToDisplayWidth(ln, max)
	}
	return out
}

func padOrTrim(s string, w int) string {
	if w <= 0 {
		return ""
	}
	vw := tuiVisibleWidth(s)
	if vw == w {
		return s
	}
	if vw < w {
		return s + strings.Repeat(" ", w-vw)
	}
	return trimToDisplayWidth(s, w)
}
