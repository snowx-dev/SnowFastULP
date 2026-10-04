// Package tuibar is the shared single-line progress bar for pre-TUI work —
// the -history pre-pass ("history · checking" / "history · validating"). Each
// binary injects its own look through Style (label/muted/count/byte styles,
// its gradient bar renderer, and its byte/rate formatters), so sfu and sfl
// render the identical layout in their own palettes without a new dependency.
//
// The bar redraws in place on one line: "\r" plus the frame, padded to the
// widest frame seen so far, exactly like the rest of the family's in-place
// redraw discipline. Finish() writes a clearing run of spaces covering that
// widest frame, so no tail of an earlier frame survives on the terminal.
// Throttling (100ms, with the completing frame always drawn) lives here in
// the display layer; hash and validation layers report unthrottled.
package tuibar

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/snowx-dev/SnowFastULP/internal/plural"
	"github.com/snowx-dev/SnowFastULP/internal/tuiframe"
)

const (
	// drawInterval matches the pre-pass throttle convention: interior
	// frames redraw at most every 100ms; the completing frame always draws.
	drawInterval = 100 * time.Millisecond
	// barTargetWidth is the width the bar segment asks for on a full-width
	// terminal; it shrinks toward barMinWidth before stats are dropped.
	barTargetWidth = 24
	barMinWidth    = 8
	// defaultWidth is the fallback when no width probe is supplied.
	defaultWidth = 80
	// segmentGap separates the title/bar/stat segments.
	segmentGap = 2
	// barSuffixWidth is the shared " 100.0%" suffix convention.
	barSuffixWidth = 7
)

// Style carries the per-binary look. Bar renders the percent bar into the
// given cell budget; Bytes and Rate format human units with the binary's own
// conventions.
type Style struct {
	Label lipgloss.Style
	Muted lipgloss.Style
	Count lipgloss.Style
	Byte  lipgloss.Style
	Bar   func(percent float64, width int) string
	Bytes func(n int64) string
	Rate  func(bps float64) string
}

// Bar is a concurrency-safe one-line progress bar. A nil Bar (or a nil
// writer) is a silent no-op, which is how callers degrade on non-TTY output.
type Bar struct {
	w          io.Writer
	style      Style
	title      string
	width      func() int
	mu         sync.Mutex
	start      time.Time
	lastAt     time.Time
	maxLen     int
	finished   bool
	filesDone  int
	filesTotal int
	bytesDone  int64
	bytesTotal int64
}

// New returns a bar rendering to w. width probes the live terminal width per
// draw (nil falls back to 80 columns).
func New(w io.Writer, style Style, title string, width func() int) *Bar {
	return &Bar{w: w, style: style, title: title, width: width}
}

// VisibleWidth measures s in terminal cells, ignoring ANSI escapes.
func VisibleWidth(s string) int {
	return tuiframe.VisibleWidth(s)
}

// Update snapshots the aggregate progress and redraws, throttled to 100ms —
// except the completing frame, which always draws.
func (b *Bar) Update(filesDone, filesTotal int, bytesDone, bytesTotal int64) {
	if b == nil || b.w == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return
	}
	b.filesDone, b.filesTotal = filesDone, filesTotal
	b.bytesDone, b.bytesTotal = bytesDone, bytesTotal
	now := time.Now()
	if b.start.IsZero() {
		b.start = now
	}
	complete := (b.filesTotal > 0 && b.filesDone >= b.filesTotal) ||
		(b.bytesTotal > 0 && b.bytesDone >= b.bytesTotal)
	if !b.lastAt.IsZero() && !complete && now.Sub(b.lastAt) < drawInterval {
		return
	}
	b.draw(now)
}

// SeqProgress adapts a sequential per-file pass — one (path, done, total)
// callback per read chunk, e.g. history.ValidateAllContext — onto the
// aggregate bar: bytes accumulate across files and a file counts done when
// the pass moves past it. The caller flushes the final completed frame with
// Update after the pass.
func (b *Bar) SeqProgress(filesTotal int, bytesTotal int64) func(path string, done, total int64) {
	var (
		cur       string
		last      int64 // cumulative bytes reported for the current file
		completed int64 // bytes from files the pass has moved past
		files     int
	)
	return func(path string, done, _ int64) {
		if b == nil {
			return
		}
		if path != cur {
			if cur != "" {
				completed += last
				files++
			}
			cur = path
			last = 0
		}
		last = done
		b.Update(files, filesTotal, completed+last, bytesTotal)
	}
}

// Finish erases the bar: a clearing run of spaces covering the widest frame
// seen, leaving the cursor at column zero. Drawing after Finish is a no-op.
func (b *Bar) Finish() {
	if b == nil || b.w == nil || b.maxLen == 0 {
		return
	}
	fmt.Fprintf(b.w, "\r%s\r", strings.Repeat(" ", b.maxLen))
	b.mu.Lock()
	b.finished = true
	b.mu.Unlock()
}

func (b *Bar) termWidth() int {
	if b.width == nil {
		return defaultWidth
	}
	if w := b.width(); w > 0 {
		return w
	}
	return defaultWidth
}

func (b *Bar) draw(now time.Time) {
	b.lastAt = now
	line := b.render(b.termWidth(), now)
	if w := VisibleWidth(line); w > b.maxLen {
		b.maxLen = w
	}
	if pad := b.maxLen - VisibleWidth(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	fmt.Fprintf(b.w, "\r%s", line)
}

func (b *Bar) percent() float64 {
	if b.bytesTotal > 0 {
		p := float64(b.bytesDone) / float64(b.bytesTotal)
		return min(max(p, 0), 1)
	}
	if b.filesTotal > 0 && b.filesDone >= b.filesTotal {
		return 1
	}
	return 0
}

// render lays out title · bar · files · bytes · rate, degrading by dropping
// the rate, shrinking the bar, dropping the bytes/files segments, dropping
// the bar, and finally truncating the title — so the frame never exceeds the
// terminal width and the title survives the longest.
func (b *Bar) render(width int, now time.Time) string {
	pct := b.percent()
	title := b.style.Label.Render(b.title)
	filesSeg := ""
	if b.filesTotal > 0 {
		filesSeg = b.style.Count.Render(fmt.Sprintf("%d/%d", b.filesDone, b.filesTotal)) +
			b.style.Muted.Render(plural.Noun(b.filesTotal, " file", " files"))
	}
	bytesSeg := b.style.Byte.Render(b.style.Bytes(b.bytesDone)) +
		b.style.Muted.Render(" / ") +
		b.style.Byte.Render(b.style.Bytes(b.bytesTotal))
	rateSeg := ""
	if !b.start.IsZero() {
		if elapsed := now.Sub(b.start); elapsed > 0 && b.style.Rate != nil {
			bps := float64(b.bytesDone) / elapsed.Seconds()
			rateSeg = b.style.Byte.Render(b.style.Rate(bps))
		}
	}

	gap := strings.Repeat(" ", segmentGap)
	keepFiles, keepBytes, keepRate, keepBar := filesSeg != "", true, rateSeg != "", true
	barW := barTargetWidth
	for {
		segs := []string{title}
		if keepBar {
			segs = append(segs, b.style.Bar(pct, barW))
		}
		if keepFiles {
			segs = append(segs, filesSeg)
		}
		if keepBytes {
			segs = append(segs, bytesSeg)
		}
		if keepRate {
			segs = append(segs, rateSeg)
		}
		line := strings.Join(segs, gap)
		if VisibleWidth(line) <= width {
			return line
		}
		switch {
		case keepRate:
			keepRate = false
		case barW > barMinWidth:
			barW--
		case keepBytes:
			keepBytes = false
		case keepFiles:
			keepFiles = false
		case keepBar:
			keepBar = false
		default:
			return tuiframe.TruncateRight(title, width)
		}
	}
}
