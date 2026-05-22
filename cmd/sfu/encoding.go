package main

import (
	"io"
	"os"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

type fileEncoding int

const (
	encUTF8 fileEncoding = iota
	encUTF16LE
	encUTF16BE
)

func (e fileEncoding) String() string {
	switch e {
	case encUTF16LE:
		return "utf-16-le"
	case encUTF16BE:
		return "utf-16-be"
	default:
		return "utf-8"
	}
}

// peeks first 3 bytes, returns enc + bom bytes to skip
func sniffEncoding(path string) (enc fileEncoding, bomBytes int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return encUTF8, 0, err
	}
	defer f.Close()

	var head [3]byte
	n, _ := io.ReadFull(f, head[:])
	if n >= 2 {
		if head[0] == 0xff && head[1] == 0xfe {
			return encUTF16LE, 2, nil
		}
		if head[0] == 0xfe && head[1] == 0xff {
			return encUTF16BE, 2, nil
		}
	}
	if n >= 3 && head[0] == 0xef && head[1] == 0xbb && head[2] == 0xbf {
		return encUTF8, 3, nil
	}
	return encUTF8, 0, nil
}

// counts raw bytes from underlying reader. used so utf-16 progress
// reports against on-disk size not decoded size. not thread-safe.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// returns a utf-8 reader. utf-8 input passes through unchanged.
// caller must have already advanced past any BOM. stateful, single-use,
// not safe across goroutines, fresh wrap per file.
func wrapReader(r io.Reader, enc fileEncoding) io.Reader {
	switch enc {
	case encUTF16LE:
		dec := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
		return transform.NewReader(r, dec)
	case encUTF16BE:
		dec := unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder()
		return transform.NewReader(r, dec)
	default:
		return r
	}
}
