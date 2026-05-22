package main

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/snowx-dev/SnowFastULP/internal/search"
	"github.com/snowx-dev/SnowFastULP/internal/zstdframe"
)

func indexActivity(metrics *search.Metrics, archiveName string) *zstdframe.Activity {
	if metrics == nil {
		return nil
	}
	return &zstdframe.Activity{
		FrameScan: func(start bool) {
			if start {
				metrics.BeginFrameScan(archiveName)
			} else {
				metrics.EndFrameScan(archiveName)
			}
		},
		Decode: func(start bool) {
			if start {
				metrics.BeginDecode(archiveName)
			} else {
				metrics.EndDecode(archiveName)
			}
		},
	}
}

// pins TUI to top, hits stream into scroll region below
type terminalLayout struct {
	mu          sync.Mutex
	enabled     bool
	reservedTop int
	scrollTop   int
	scrollBot   int
	hitsStarted bool
}

func (l *terminalLayout) Enable() {
	if l == nil {
		return
	}
	l.enabled = true
}

func (l *terminalLayout) SetReservedTop(n int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n < 1 {
		n = 1
	}
	l.reservedTop = n
}

func (l *terminalLayout) DrawHeader(draw func()) {
	if l == nil || !l.enabled {
		draw()
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.syncScrollRegionLocked()
	fmt.Fprint(os.Stderr, "\033[s")
	draw()
	fmt.Fprint(os.Stderr, "\033[u")
}

func (l *terminalLayout) WriteHits(w io.Writer, p []byte) (int, error) {
	if l == nil || !l.enabled {
		return w.Write(p)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.syncScrollRegionOn(os.Stdout)
	if !l.hitsStarted && l.reservedTop > 0 {
		fmt.Fprintf(os.Stdout, "\033[%d;1H", l.reservedTop+1)
		l.hitsStarted = true
	}
	return w.Write(p)
}

func (l *terminalLayout) Reset() {
	if l == nil || !l.enabled {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(os.Stderr, "\033[r")
	l.scrollTop = 0
	l.scrollBot = 0
	l.hitsStarted = false
}

func (l *terminalLayout) syncScrollRegionLocked() {
	l.syncScrollRegionOn(os.Stderr)
}

func (l *terminalLayout) syncScrollRegionOn(out *os.File) {
	h := termHeight()
	top := l.reservedTop
	if top < 1 {
		top = 1
	}
	if h <= top {
		return
	}
	bot := h
	if l.scrollTop == top+1 && l.scrollBot == bot {
		return
	}
	fmt.Fprintf(out, "\033[%d;%dr", top+1, bot)
	l.scrollTop = top + 1
	l.scrollBot = bot
}

type viewportStdout struct {
	w io.Writer
	l *terminalLayout
}

func (v *viewportStdout) Write(p []byte) (int, error) {
	return v.l.WriteHits(v.w, p)
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func newHitViewportWriter(w io.Writer, layout *terminalLayout) io.Writer {
	if layout == nil || !layout.enabled {
		return w
	}
	return &viewportStdout{w: w, l: layout}
}
