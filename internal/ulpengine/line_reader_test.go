package ulpengine

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadBoundedLineRejectsOversizeAndContinues(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("abcdef\nok\n"), 4)

	line, n, tooLong, err := readBoundedLine(br, 3)
	if err != nil {
		t.Fatalf("first read err = %v", err)
	}
	if !tooLong {
		t.Fatalf("first read tooLong = false, want true")
	}
	if line != "" {
		t.Fatalf("oversize line = %q, want empty", line)
	}
	if n != int64(len("abcdef\n")) {
		t.Fatalf("oversize consumed = %d, want %d", n, len("abcdef\n"))
	}

	line, n, tooLong, err = readBoundedLine(br, 3)
	if err != nil {
		t.Fatalf("second read err = %v", err)
	}
	if tooLong {
		t.Fatalf("second read tooLong = true, want false")
	}
	if line != "ok\n" {
		t.Fatalf("second line = %q, want %q", line, "ok\n")
	}
	if n != int64(len("ok\n")) {
		t.Fatalf("second consumed = %d, want %d", n, len("ok\n"))
	}

	_, _, _, err = readBoundedLine(br, 3)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("final err = %v, want EOF", err)
	}
}

func TestReadBoundedLineAllowsMaxPlusLineEnding(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("abcd\r\n"))

	line, n, tooLong, err := readBoundedLine(br, 6)
	if err != nil {
		t.Fatalf("read err = %v", err)
	}
	if tooLong {
		t.Fatalf("tooLong = true, want false")
	}
	if line != "abcd\r\n" {
		t.Fatalf("line = %q, want %q", line, "abcd\\r\\n")
	}
	if n != int64(len("abcd\r\n")) {
		t.Fatalf("consumed = %d, want %d", n, len("abcd\r\n"))
	}
}
