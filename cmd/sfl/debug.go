package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// debugLogger writes structured, timestamped events to a log file when -debug
// is set. It records provenance (source path + credential counts) but never raw
// credential values.
type debugLogger struct {
	mu sync.Mutex
	f  *os.File
}

func newDebugLogger(cfg runConfig) *debugLogger {
	if !cfg.Debug {
		return nil
	}
	dir := cfg.OutputDir
	if dir == "" {
		dir = cfg.LibraryDir
	}
	if dir == "" {
		dir = "."
	}
	_ = os.MkdirAll(dir, 0o755)
	started := cfg.Started
	if started.IsZero() {
		started = time.Now()
	}
	name := filepath.Join(dir, "sfl_debug_"+started.Format("20060102_150405")+".log")
	f, err := os.Create(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sfl: debug log disabled: %v\n", err)
		return nil
	}
	d := &debugLogger{f: f}
	d.Event("sfl debug log started")
	return d
}

func (d *debugLogger) Event(format string, args ...any) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.f, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

func (d *debugLogger) Close() {
	if d == nil || d.f == nil {
		return
	}
	_ = d.f.Close()
}
