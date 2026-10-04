package sflog

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestWriteULPLinesDedupsWithinRun(t *testing.T) {
	creds := []Credential{
		{URL: "https://a.example.com/login", Username: "u", Password: "p"},
		{URL: "https://a.example.com/login", Username: "u", Password: "p"},
		{URL: "https://b.example.com", Username: "u2", Password: "p2"},
	}
	var out bytes.Buffer
	stats, err := WriteULPLines(&out, creds, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Emitted != 2 || stats.Duplicates != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	want := "a.example.com/login:u:p\nb.example.com:u2:p2\n"
	if out.String() != want {
		t.Fatalf("out = %q want %q", out.String(), want)
	}
}

// failOnNWriter accepts writes until its Nth call, then fails. With output
// small enough to stay inside bufio's 4096-byte buffer, every WriteString
// succeeds in memory and the only underlying write is the final flush.
type failOnNWriter struct {
	failOn int
	calls  int
}

func (w *failOnNWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls >= w.failOn {
		return len(p), errors.New("disk full")
	}
	return len(p), nil
}

// All buffered bytes reach the supplied writer on success: a flush failure
// must be returned (with the stats accumulated so far), not silently dropped
// — otherwise the tail of the output is lost while the caller sees success.
func TestWriteULPLinesReturnsFlushError(t *testing.T) {
	creds := []Credential{
		{URL: "https://a.example.com/login", Username: "u", Password: "p"},
		{URL: "https://b.example.com", Username: "u2", Password: "p2"},
	}
	fw := &failOnNWriter{failOn: 1} // fails on the first underlying write = the flush
	stats, err := WriteULPLines(fw, creds, false)
	if err == nil {
		t.Fatal("flush failure must be returned, got nil error")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the flush failure", err)
	}
	if stats.Emitted != 2 || stats.Seen != 2 {
		t.Fatalf("stats = %+v, want buffered writes reflected with the error", stats)
	}
}

// A direct write error still returns immediately: no extra flush attempt after
// the failure.
func TestWriteULPLinesWriteErrorNoDoubleFlush(t *testing.T) {
	// One credential whose line exceeds bufio's buffer so every WriteString
	// flushes through to the underlying writer.
	big := strings.Repeat("x", 5000)
	creds := []Credential{
		{URL: "https://" + big + ".example.com/l", Username: "u", Password: "p"},
		{URL: "https://" + big + "2.example.com/l", Username: "u", Password: "p"},
		{URL: "https://" + big + "3.example.com/l", Username: "u", Password: "p"},
	}
	fw := &failOnNWriter{failOn: 2} // first flush ok, second write fails
	_, err := WriteULPLines(fw, creds, false)
	if err == nil {
		t.Fatal("write failure must be returned")
	}
	if fw.calls != 2 {
		t.Fatalf("underlying write calls = %d, want 2 (no flush after the write error)", fw.calls)
	}
}
