package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snowx-dev/SnowFastULP/internal/ulpengine"
)

func TestEstimateNeeds(t *testing.T) {
	cases := []struct {
		name     string
		total    int64
		compress bool
		fastPath bool
		wantOut  int64
		wantTemp int64
	}{
		{"plain bucketed", 1000, false, false, 1000, 1000},
		{"plain fastpath", 1000, false, true, 1000, 0},
		{"zst bucketed", 1000, true, false, 200, 1000},
		{"zst fastpath", 1000, true, true, 200, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &ulpengine.Resolved{
				Cfg:         ulpengine.Config{Compress: c.compress},
				TotalInputs: c.total,
				UseFastPath: c.fastPath,
			}
			gotOut, gotTemp := estimateNeeds(r)
			if gotOut != c.wantOut {
				t.Errorf("out=%d want %d", gotOut, c.wantOut)
			}
			if gotTemp != c.wantTemp {
				t.Errorf("temp=%d want %d", gotTemp, c.wantTemp)
			}
		})
	}
}

func TestBuildDiskWarningEmptyWhenEnoughSpace(t *testing.T) {
	// zero-need volumes always pass
	if w := buildDiskWarning(t.TempDir(), t.TempDir(), 0, 0); w != "" {
		t.Errorf("expected empty warning for zero need, got %q", w)
	}
}

func TestPreflightCheckProceedsWhenNoWarning(t *testing.T) {
	r := &ulpengine.Resolved{TotalInputs: 0}
	var out bytes.Buffer
	ok, err := preflightCheck(context.Background(), r, true, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !ok {
		t.Errorf("expected ok=true for zero-input, got false")
	}
	if out.Len() != 0 {
		t.Errorf("expected no output, got %q", out.String())
	}
}

func TestPreflightNonInteractiveAutoContinuesWithWarning(t *testing.T) {
	r := bigOverflowResolved()
	var out bytes.Buffer
	ok, err := preflightCheck(context.Background(), r, false, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !ok {
		t.Errorf("expected non-interactive auto-continue, got ok=false")
	}
	if !strings.Contains(out.String(), "warning:") {
		t.Errorf("expected warning printed, got: %q", out.String())
	}
	if !strings.Contains(out.String(), "not a tty") {
		t.Errorf("expected non-tty notice, got: %q", out.String())
	}
}

// 2^60 bytes vs os.TempDir, always trips the warning branch
func bigOverflowResolved() *ulpengine.Resolved {
	tmp := os.TempDir()
	return &ulpengine.Resolved{
		Cfg:         ulpengine.Config{Output: filepath.Join(tmp, "sfu-preflight-test-output.txt")},
		TotalInputs: 1 << 60,
		TempDir:     tmp,
		UseFastPath: true,
	}
}

func TestPreflightPromptUserResponse(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    bool
		wantErr bool
	}{
		{"enter accepts", "\n", true, false},
		{"y accepts", "y\n", true, false},
		{"Y accepts", "Y\n", true, false},
		{"yes accepts", "yes\n", true, false},
		{"n declines", "n\n", false, false},
		{"NO declines", "NO\n", false, false},
		// bad answer then good one, re-prompts and accepts
		{"garbage then yes", "maybe\ny\n", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			ok, err := preflightCheck(context.Background(), bigOverflowResolved(), true, strings.NewReader(c.input), &out)
			if c.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("err=%v", err)
			}
			if ok != c.want {
				t.Errorf("ok=%v want %v (output: %q)", ok, c.want, out.String())
			}
			if !strings.Contains(out.String(), "warning:") {
				t.Errorf("expected a warning, got: %q", out.String())
			}
		})
	}
}

func TestPreflightRejectsGarbageThriceThenAborts(t *testing.T) {
	var out bytes.Buffer
	ok, err := preflightCheck(context.Background(), bigOverflowResolved(), true,
		strings.NewReader("foo\nbar\nbaz\n"), &out)
	if err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if ok {
		t.Fatal("expected ok=false on garbage")
	}
	if !strings.Contains(err.Error(), "invalid response") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPreflightCancelOnCtxDone(t *testing.T) {
	// reader blocks forever, prompt stuck on stdin until ctx fires
	pr := newBlockingReader(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	var out bytes.Buffer
	ok, err := preflightCheck(ctx, bigOverflowResolved(), true, pr, &out)
	if ok {
		t.Fatal("expected ok=false on ctx cancel")
	}
	if err == nil || err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func newBlockingReader(t *testing.T) *blockingReader {
	t.Helper()
	return &blockingReader{}
}

type blockingReader struct{}

func (b *blockingReader) Read(p []byte) (int, error) {
	// blocks forever, test cancels via ctx
	select {}
}
