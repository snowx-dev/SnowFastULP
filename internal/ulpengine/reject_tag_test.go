package ulpengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// D6: a line whose only parse failure is the 64-char password cap must carry
// the password>64 tag in the -debug-reject file and tick its own counter,
// instead of vanishing into generic malformed. A normal reject stays
// untagged. Both read paths run the same classify-and-record branch.
func TestDebugRejectPasswordTooLongTagged(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fastPathOff bool
	}{
		{name: "fast path", fastPathOff: false},
		{name: "bucketed path", fastPathOff: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			in := filepath.Join(d, "in.txt")
			longPw := "https://a.example.com:user:" + strings.Repeat("x", 65)
			writeFile(t, in,
				longPw+"\n"+
					"not-a-line\n",
			)
			rejPath := filepath.Join(d, "rej.txt")
			rr, err := NewRejectRecorder(rejPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rr.Close() })

			cfg := Config{
				Inputs:       []string{in},
				Output:       filepath.Join(d, "out.txt"),
				TempDir:      filepath.Join(d, "shards"),
				Workers:      1,
				DedupWorkers: 1,
				Buckets:      4,
				ChunkBytes:   1 << 20,
				FastPathOff:  tc.fastPathOff,
				Reject:       rr,
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
			_ = rr.Close()
			raw, err := os.ReadFile(rejPath)
			if err != nil {
				t.Fatal(err)
			}
			s := string(raw)
			if !strings.Contains(s, "[password>64] "+longPw) {
				t.Fatalf("%s: reject file missing the tagged over-cap line: %q", tc.name, s)
			}
			// The generic reject stays untagged: its raw field is not
			// bracket-prefixed.
			if !strings.Contains(s, "\tnot-a-line\n") {
				t.Fatalf("%s: reject file missing the untagged generic reject: %q", tc.name, s)
			}
			if strings.Count(s, "[password>64]") != 1 {
				t.Fatalf("%s: tag must appear exactly once: %q", tc.name, s)
			}
			if got := m.LinesRejected.Load(); got != 2 {
				t.Fatalf("%s: linesRejected = %d, want 2", tc.name, got)
			}
			if got := m.LinesPasswordTooLong.Load(); got != 1 {
				t.Fatalf("%s: linesPasswordTooLong = %d, want 1", tc.name, got)
			}
			if got := m.LinesMalformed.Load(); got != 1 {
				t.Fatalf("%s: linesMalformed = %d, want 1 (generic reject stays generic)", tc.name, got)
			}
		})
	}
}

// The classifier tags only the password cap: other finishParse rules and
// unrecognized shapes stay generic.
func TestRejectTagOnlyNamesPasswordCap(t *testing.T) {
	if got := rejectTag("https://a.example.com:user:" + strings.Repeat("x", 65)); got != rejectTagPasswordTooLong {
		t.Fatalf("rejectTag(>64 password) = %q, want %q", got, rejectTagPasswordTooLong)
	}
	if got := rejectTag("https://a.example.com:user:p"); got != "" {
		t.Fatalf("rejectTag(valid) = %q, want empty", got)
	}
	if got := rejectTag("localhost:user:p"); got != "" {
		t.Fatalf("rejectTag(host-without-dot rule) = %q, want generic empty", got)
	}
	if got := rejectTag("not-a-line"); got != "" {
		t.Fatalf("rejectTag(garbage) = %q, want generic empty", got)
	}
	if got := rejectTag(""); got != "" {
		t.Fatalf("rejectTag(empty) = %q, want empty", got)
	}
}
