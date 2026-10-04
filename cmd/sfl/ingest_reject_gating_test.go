package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Gating matrix for the ingest reject recorder: -debug-reject owns the reject
// artifact independently of -debug, mirroring sfu's -debug-reject.
//
//	-debug alone         -> ingest debug log, NO reject file
//	-debug-reject alone  -> reject file, NO ingest debug log
//	both                 -> both artifacts
func TestIngestRejectRecorderGating(t *testing.T) {
	cases := []struct {
		name        string
		debug       bool
		debugReject bool
	}{
		{"debug only", true, false},
		{"debug-reject only", false, true},
		{"both", true, true},
		{"neither", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			cfg := runConfig{Debug: tc.debug, DebugReject: tc.debugReject}
			elog := newIngestDebugLog(cfg)
			if tc.debug != (elog != nil) {
				t.Fatalf("newIngestDebugLog nil=%v, want debug=%v", elog == nil, tc.debug)
			}
			if elog != nil {
				_ = elog.Close()
			}
			rr := newIngestRejectRecorder(cfg)
			if tc.debugReject != (rr != nil) {
				t.Fatalf("newIngestRejectRecorder nil=%v, want debug-reject=%v", rr == nil, tc.debugReject)
			}
			if rr != nil {
				_ = rr.Close()
			}
			debugLogs, err := filepath.Glob(filepath.Join(dir, "sfl_ingest_debug_*.log"))
			if err != nil {
				t.Fatal(err)
			}
			rejects, err := filepath.Glob(filepath.Join(dir, "sfl_ingest_rejected_*.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if want := b2i(tc.debug); len(debugLogs) != want {
				t.Fatalf("got %d ingest debug logs (%v), want %d", len(debugLogs), debugLogs, want)
			}
			if want := b2i(tc.debugReject); len(rejects) != want {
				t.Fatalf("got %d ingest reject files (%v), want %d", len(rejects), rejects, want)
			}
			// Nothing may leak into CWD beyond the expected artifacts.
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(entries); n != len(debugLogs)+len(rejects) {
				t.Fatalf("unexpected files in CWD: %v", entries)
			}
		})
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
