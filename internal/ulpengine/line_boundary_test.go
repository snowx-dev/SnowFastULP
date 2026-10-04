package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The documented 4096-char input-line limit must hold exactly at the
// boundary on both read paths: the reader admits maxParsedLineLen+2 raw bytes
// (CRLF framing included) and the parser admits maxParsedLineLen chars after
// trailing-CR/LF trim. Lines are host-padded so the password stays within the
// 64-char cap — the length boundary must not depend on which field grows.
//
// D5 regression pin: 4095 passes, 4096 passes (LF and CRLF), 4097 fails.
func TestLineBoundary4096(t *testing.T) {
	d := t.TempDir()
	in := filepath.Join(d, "in.txt")

	// A valid strict line of exactly n chars: <pad>.example.com:alice:pw123.
	line := func(n int) string {
		tail := ".example.com:alice:pw123" // len 24
		return strings.Repeat("a", n-len(tail)) + tail
	}
	var body []byte
	body = append(body, line(4095)...) // LF 4095: pass
	body = append(body, '\n')
	body = append(body, line(4096)...) // LF 4096: pass
	body = append(body, '\n')
	body = append(body, line(4096)...) // CRLF 4096: pass (4098 raw bytes)
	body = append(body, '\r', '\n')
	body = append(body, line(4097)...) // LF 4097: parse reject (>4096 after trim)
	body = append(body, '\n')
	body = append(body, line(4097)...) // CRLF 4097: reader tooLong (4099 raw)
	body = append(body, '\r', '\n')
	if err := os.WriteFile(in, body, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		fastPathOff bool
	}{
		{name: "fast path", fastPathOff: false},
		{name: "bucketed path", fastPathOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(d, "out-"+tc.name+".txt")
			cfg := Config{
				Inputs:       []string{in},
				Output:       out,
				TempDir:      filepath.Join(d, "shards"),
				Workers:      1,
				DedupWorkers: 1,
				Buckets:      4,
				ChunkBytes:   1 << 20,
				FastPathOff:  tc.fastPathOff,
			}
			r, err := Resolve(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r.UseFastPath = !tc.fastPathOff
			m := &Metrics{TotalInputBytes: r.TotalInputs}
			if err := Run(context.Background(), r, m); err != nil {
				t.Fatal(err)
			}
			if got := m.LinesRead.Load(); got != 5 {
				t.Fatalf("%s: linesRead = %d, want 5", tc.name, got)
			}
			if got := m.LinesRejected.Load(); got != 2 {
				t.Fatalf("%s: linesRejected = %d, want 2 (4097 LF parse-reject + 4097 CRLF too-long)", tc.name, got)
			}
			if got := m.LinesTooLong.Load(); got != 1 {
				t.Fatalf("%s: linesTooLong = %d, want 1 (CRLF 4097 exceeds the 4098 raw-byte admission)", tc.name, got)
			}
			if got := m.LinesMalformed.Load(); got != 1 {
				t.Fatalf("%s: linesMalformed = %d, want 1 (LF 4097 parses >4096 after trim)", tc.name, got)
			}
			// 4095, 4096 LF, and CRLF 4096 all parse; the two 4096 lines dedup
			// to one key, so unique = 2.
			if got := m.LinesAccepted.Load(); got != 3 {
				t.Fatalf("%s: linesAccepted = %d, want 3 (4095 + 4096 LF + 4096 CRLF)", tc.name, got)
			}
			if got := m.LinesUnique.Load(); got != 2 {
				t.Fatalf("%s: linesUnique = %d, want 2 (the CRLF twin dedups)", tc.name, got)
			}
		})
	}
}
