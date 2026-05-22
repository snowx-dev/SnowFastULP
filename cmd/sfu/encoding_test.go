package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// every recognised BOM + no-BOM fallback. missed BOM would let raw
// UTF-16 reach the parser and crater recall
func TestSniffEncoding(t *testing.T) {
	cases := []struct {
		name     string
		bytes    []byte
		wantEnc  fileEncoding
		wantSkip int
	}{
		{"utf16le_bom", []byte{0xff, 0xfe, 'h', 0, 'i', 0}, encUTF16LE, 2},
		{"utf16be_bom", []byte{0xfe, 0xff, 0, 'h', 0, 'i'}, encUTF16BE, 2},
		{"utf8_bom", []byte{0xef, 0xbb, 0xbf, 'h', 'i'}, encUTF8, 3},
		{"ascii_no_bom", []byte("hello world"), encUTF8, 0},
		{"empty", []byte{}, encUTF8, 0},
		{"one_byte", []byte{0xff}, encUTF8, 0},
	}
	dir := t.TempDir()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(dir, c.name+".txt")
			if err := os.WriteFile(p, c.bytes, 0o644); err != nil {
				t.Fatal(err)
			}
			enc, skip, err := sniffEncoding(p)
			if err != nil {
				t.Fatalf("sniffEncoding err: %v", err)
			}
			if enc != c.wantEnc || skip != c.wantSkip {
				t.Errorf("sniffEncoding = (%v, %d), want (%v, %d)", enc, skip, c.wantEnc, c.wantSkip)
			}
		})
	}
}

// UTF-16 LE round-trip via transform.Reader, same path shard worker uses
func TestWrapReaderUTF16LE(t *testing.T) {
	// "hi\n" UTF-16 LE = h\0 i\0 \n\0
	raw := []byte{'h', 0, 'i', 0, '\n', 0}
	r := wrapReader(bytes.NewReader(raw), encUTF16LE)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi\n" {
		t.Errorf("decoded = %q, want %q", got, "hi\n")
	}
}

// regression boundary for encoding detection. real-world UTF-16 dumps
// stop parsing if this breaks
func TestParsePipelineUTF16File(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "input.txt")

	// UTF-16 LE w/ BOM + CRLF, matches Windows/openbullet exports
	utf8Lines := []string{
		"https://example.com:user1@example.com:secret123",
		"http://www.test.com/login:alice:pw",
		"user2@example.com:pass2:https://lpu.example.com/login", // LPU
		"foo.bar.com:8080/path:bob:tok",
	}
	var raw bytes.Buffer
	raw.Write([]byte{0xff, 0xfe}) // UTF-16 LE BOM
	for _, line := range utf8Lines {
		for _, ch := range line {
			raw.WriteByte(byte(ch))
			raw.WriteByte(0)
		}
		raw.Write([]byte{'\r', 0, '\n', 0})
	}
	if err := os.WriteFile(src, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if enc, skip, _ := sniffEncoding(src); enc != encUTF16LE || skip != 2 {
		t.Fatalf("sniff = (%v, %d), want (utf-16-le, 2)", enc, skip)
	}

	outFile := filepath.Join(dir, "out.txt")
	tempParent := filepath.Join(dir, "stage")
	r, err := resolvePipelineConfig(pipelineConfig{
		Inputs:      []string{src},
		Output:      outFile,
		TempDir:     tempParent,
		FastPathOff: true,
		Buckets:     4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), &resolved{
		cfg:          r.cfg,
		totalInputs:  r.totalInputs,
		mem:          r.mem,
		bucketCount:  4,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      tempParent,
	}, &metrics{}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	wantFragments := []string{
		"example.com:user1@example.com:secret123",
		"www.test.com/login:alice:pw",
		"lpu.example.com/login:user2@example.com:pass2",
		"foo.bar.com:8080/path:bob:tok",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(got, frag) {
			t.Errorf("missing %q in output:\n%s", frag, got)
		}
	}
}

// same UTF-16 input via single-file fast path, both paths must agree
func TestFastPathUTF16File(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "input.txt")

	var raw bytes.Buffer
	raw.Write([]byte{0xff, 0xfe})
	for _, ch := range "https://example.com:user:secret\n" {
		raw.WriteByte(byte(ch))
		raw.WriteByte(0)
	}
	if err := os.WriteFile(src, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	outFile := filepath.Join(dir, "out.txt")
	r, err := resolvePipelineConfig(pipelineConfig{
		Inputs: []string{src},
		Output: outFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), &resolved{
		cfg:          r.cfg,
		totalInputs:  r.totalInputs,
		mem:          r.mem,
		bucketCount:  1,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      filepath.Join(dir, "stage"),
	}, &metrics{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "example.com:user:secret") {
		t.Errorf("fast-path UTF-16 output missing credential, got:\n%s", body)
	}
}

// -no-encoding-sniff must really disable UTF-16 decoding. w/o the flag
// UTF-16 LE decodes fine, w/ the flag NUL-padded chars must reject and
// no credential should reach output
func TestNoEncodingSniffSkipsUTF16(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "input.txt")

	var raw bytes.Buffer
	raw.Write([]byte{0xff, 0xfe})
	for _, ch := range "https://example.com:user:secret\n" {
		raw.WriteByte(byte(ch))
		raw.WriteByte(0)
	}
	if err := os.WriteFile(src, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	outFile := filepath.Join(dir, "out.txt")
	r, err := resolvePipelineConfig(pipelineConfig{
		Inputs:          []string{src},
		Output:          outFile,
		FastPathOff:     true,
		Buckets:         2,
		NoEncodingSniff: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), &resolved{
		cfg:          r.cfg,
		totalInputs:  r.totalInputs,
		mem:          r.mem,
		bucketCount:  2,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      filepath.Join(dir, "stage"),
	}, &metrics{}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(outFile)
	if err != nil {
		// run() may skip output file when nothing accepted, ENOENT = pass
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	if strings.Contains(string(body), "example.com:user:secret") {
		t.Errorf("with -no-encoding-sniff, UTF-16 input must NOT decode; got: %q", body)
	}
}

// UTF-8 BOM must be stripped before parser sees the line, else EF BB BF
// would glue to head and break the URL regex
func TestPipelineUTF8BOMFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "input.txt")
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte("https://example.com:user:secret\n")...)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(dir, "out.txt")
	r, err := resolvePipelineConfig(pipelineConfig{
		Inputs:      []string{src},
		Output:      outFile,
		FastPathOff: true,
		Buckets:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), &resolved{
		cfg:          r.cfg,
		totalInputs:  r.totalInputs,
		mem:          r.mem,
		bucketCount:  2,
		workers:      1,
		dedupWorkers: 1,
		chunkBytes:   1 << 20,
		tempDir:      filepath.Join(dir, "stage"),
	}, &metrics{}); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "example.com:user:secret") {
		t.Errorf("UTF-8 BOM file: expected credential line, got:\n%q", out)
	}
	// BOM bytes must not survive into output
	if bytes.Contains(out, []byte{0xef, 0xbb, 0xbf}) {
		t.Errorf("UTF-8 BOM bytes leaked into output: %q", out)
	}
}
